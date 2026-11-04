// Package transfer gère les transferts : brides d'upload côté serveur, file
// de téléchargement et reprise côté client (jalon J3).
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

// burst borne la taille de chaque lecture bridée : assez gros pour peu
// d'attentes par seconde à 1 Gb/s, assez petit pour un débit régulier.
const burst = 256 << 10

// MaxUploadsRetry est le délai proposé quand toutes les places d'upload sont prises.
const MaxUploadsRetry = 60 * time.Second

// Uploads applique les limites d'upload du nœud : débit total partagé par tous
// les uploads, nombre d'uploads simultanés, créneau horaire. Tout se règle à chaud.
type Uploads struct {
	limiter *rate.Limiter
	now     func() time.Time

	mu     sync.Mutex
	limit  int64
	max    int
	window *config.Window
	active int
}

// NewUploads crée les brides. limit en octets/s (0 = illimité), window nil = toujours ouvert.
func NewUploads(limit int64, max int, window *config.Window) *Uploads {
	u := &Uploads{limiter: rate.NewLimiter(rate.Inf, burst), now: time.Now}
	u.SetLimit(limit)
	u.SetMax(max)
	u.SetWindow(window)
	return u
}

// SetLimit change le débit total (0 = illimité).
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

// SetMax change le nombre d'uploads simultanés. Les uploads en cours au-delà
// du nouveau maximum se terminent normalement.
func (u *Uploads) SetMax(n int) {
	if n < 1 {
		n = 1
	}
	u.mu.Lock()
	u.max = n
	u.mu.Unlock()
}

// SetWindow change le créneau (nil = toujours ouvert).
func (u *Uploads) SetWindow(w *config.Window) {
	u.mu.Lock()
	u.window = w
	u.mu.Unlock()
}

// Limits renvoie les réglages courants et le nombre d'uploads en cours.
func (u *Uploads) Limits() (limit int64, max int, window *config.Window, active int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.limit, u.max, u.window, u.active
}

// Acquire réserve une place d'upload. Si c'est impossible, ok est faux et
// retryAfter dit quand réessayer : fin du créneau fermé, ou 60 s si toutes les
// places sont prises.
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

// Reader bride r sur le débit total. ctx interrompt l'attente si le client part.
func (u *Uploads) Reader(ctx context.Context, r io.ReadSeeker) io.ReadSeeker {
	return &limitedReader{ctx: ctx, r: r, l: u.limiter}
}

type limitedReader struct {
	ctx context.Context
	r   io.ReadSeeker
	l   *rate.Limiter
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	if len(p) > burst {
		p = p[:burst]
	}
	n, err := lr.r.Read(p)
	if n > 0 {
		if werr := lr.l.WaitN(lr.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (lr *limitedReader) Seek(off int64, whence int) (int64, error) {
	return lr.r.Seek(off, whence)
}
