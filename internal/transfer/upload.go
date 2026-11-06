// Package transfer handles transfers: upload limits on the serving side, the
// download queue and resuming on the receiving side.
package transfer

import (
	"context"
	"io"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/DB-CL/cercle/internal/config"
)

// burst caps the size of each throttled read: large enough for few waits per
// second at 1 Gb/s, small enough for a steady rate.
const burst = 256 << 10

// MtimeHeader gives the exact modification time (Unix nanoseconds) of a
// served file: Last-Modified only has one-second precision.
const MtimeHeader = "X-Cercle-Mtime"

// MaxUploadsRetry is the delay suggested when every upload slot is taken.
const MaxUploadsRetry = 60 * time.Second

// Uploads applies the node's upload limits: total rate shared by every upload,
// number of concurrent uploads, time window. Everything is adjustable live.
type Uploads struct {
	limiter *rate.Limiter
	now     func() time.Time

	mu     sync.Mutex
	limit  int64
	max    int
	window *config.Window
	active int
}

// NewUploads creates the limits. limit in bytes/s (0 = unlimited), window nil = always open.
func NewUploads(limit int64, max int, window *config.Window) *Uploads {
	u := &Uploads{limiter: rate.NewLimiter(rate.Inf, burst), now: time.Now}
	u.SetLimit(limit)
	u.SetMax(max)
	u.SetWindow(window)
	return u
}

// SetLimit changes the total rate (0 = unlimited).
func (u *Uploads) SetLimit(bps int64) {
	u.mu.Lock()
	u.limit = bps
	u.mu.Unlock()
	if bps <= 0 {
		u.limiter.SetLimit(rate.Inf)
		return
	}
	u.limiter.SetLimit(rate.Limit(bps))
}

// SetMax changes the number of concurrent uploads. Uploads in progress beyond
// the new maximum finish normally.
func (u *Uploads) SetMax(n int) {
	if n < 1 {
		n = 1
	}
	u.mu.Lock()
	u.max = n
	u.mu.Unlock()
}

// SetWindow changes the window (nil = always open).
func (u *Uploads) SetWindow(w *config.Window) {
	u.mu.Lock()
	u.window = w
	u.mu.Unlock()
}

// Limits returns the current settings and the number of uploads in progress.
func (u *Uploads) Limits() (limit int64, max int, window *config.Window, active int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.limit, u.max, u.window, u.active
}

// Acquire reserves an upload slot. If impossible, ok is false and retryAfter
// says when to retry: when the closed window opens, or 60 s if every slot is
// taken.
func (u *Uploads) Acquire() (release func(), retryAfter time.Duration, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	if u.window != nil && !u.window.Contains(now) {
		wait := u.window.NextOpen(now).Sub(now)
		return nil, time.Duration(math.Ceil(wait.Seconds())) * time.Second, false
	}
	if u.active >= u.max {
		return nil, MaxUploadsRetry, false
	}
	u.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			u.active--
			u.mu.Unlock()
		})
	}, 0, true
}

// Reader throttles r to the total rate. ctx interrupts the wait if the client leaves.
func (u *Uploads) Reader(ctx context.Context, r io.ReadSeeker) io.ReadSeeker {
	return &limitedReader{ctx: ctx, r: r, l: u.limiter}
}

type limitedReader struct {
	ctx context.Context
	r   io.ReadSeeker
	l   *rate.Limiter
}

func (lr *limitedReader) Read(p []byte) (int, error) { return throttledRead(lr.ctx, lr.l, lr.r, p) }

func (lr *limitedReader) Seek(off int64, whence int) (int64, error) {
	return lr.r.Seek(off, whence)
}

// throttledRead reads at most burst bytes then waits for the matching tokens.
func throttledRead(ctx context.Context, l *rate.Limiter, r io.Reader, p []byte) (int, error) {
	if len(p) > burst {
		p = p[:burst]
	}
	n, err := r.Read(p)
	if n > 0 {
		if werr := l.WaitN(ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}
