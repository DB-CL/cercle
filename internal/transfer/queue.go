package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/time/rate"

	"github.com/DB-CL/cercle/internal/config"
	"github.com/DB-CL/cercle/internal/index"
	"github.com/DB-CL/cercle/internal/store"
)

// Status is the state of a queue item.
type Status string

// Item states.
const (
	Queued  Status = "queued"
	Active  Status = "active"
	Paused  Status = "paused"
	Waiting Status = "waiting" // peer unavailable, retry scheduled
	Done    Status = "done"
	Failed  Status = "error"
)

// Item is a remote file to download.
type Item struct {
	ID          int64     `json:"id"`
	PeerID      string    `json:"peer_id"`
	PeerName    string    `json:"peer_name"`
	RemotePath  string    `json:"remote_path"`
	DestPath    string    `json:"dest_path"`
	Size        int64     `json:"size"`
	MTimeNano   int64     `json:"mtime_ns"`
	ETag        string    `json:"-"`
	Status      Status    `json:"status"`
	BytesDone   int64     `json:"bytes_done"`
	Priority    int       `json:"priority"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Rate        float64   `json:"rate,omitempty"` // bytes/s, active items only
}

// Peer is a reachable peer: its identity and tunnel address.
type Peer struct {
	ID   string
	Name string
	IP   netip.Addr
}

// Remote talks to the other nodes (implemented by peerapi.Client).
type Remote interface {
	Tree(ctx context.Context, peer netip.Addr, dir string, fn func(index.Entry) error) error
	Download(ctx context.Context, peer netip.Addr, path string, offset int64, etag string) (*http.Response, error)
}

// statusError is a non-2xx HTTP response from a peer (peerapi.StatusError).
type statusError interface {
	HTTPStatus() int
	RetryDelay() time.Duration
}

// Options configures a Manager.
type Options struct {
	Store       *store.Store
	Remote      Remote
	Peers       func(id string) (Peer, bool)
	Dir         string // downloads folder
	Concurrency int
	Limit       int64 // bytes/s, 0 = unlimited
	Window      *config.Window
	Logger      *slog.Logger
	OnChange    func() // called on every state change (interface SSE stream)

	// BackoffBase and BackoffMax: 30 s and 15 min by default, shorter in tests.
	BackoffBase, BackoffMax time.Duration
}

// Manager drains the queue in the background.
type Manager struct {
	opt     Options
	log     *slog.Logger
	limiter *rate.Limiter
	now     func() time.Time
	wake    chan struct{}

	mu          sync.Mutex
	concurrency int
	window      *config.Window
	jobs        map[int64]*job
	busy        map[string]bool // peers with a transfer in progress (at most 1 per peer)
	wg          sync.WaitGroup
}

type stopReason int32

const (
	stopNone stopReason = iota
	stopPause
	stopRemove
	stopRemovePart
)

type job struct {
	cancel      context.CancelFunc
	peer        string
	done        atomic.Int64 // bytes present in the partial file
	startOffset atomic.Int64 // bytes already there when this attempt started
	start       atomic.Int64 // attempt start, Unix nanoseconds
	reason      atomic.Int32
}

// NewManager prepares the queue; Run drives it.
func NewManager(opt Options) *Manager {
	if opt.BackoffBase == 0 {
		opt.BackoffBase = 30 * time.Second
	}
	if opt.BackoffMax == 0 {
		opt.BackoffMax = 15 * time.Minute
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	m := &Manager{
		opt:     opt,
		log:     opt.Logger.With("module", "transfer"),
		limiter: rate.NewLimiter(rate.Inf, burst),
		now:     time.Now,
		wake:    make(chan struct{}, 1),
		jobs:    map[int64]*job{},
		busy:    map[string]bool{},
	}
	m.SetConcurrency(opt.Concurrency)
	m.SetLimit(opt.Limit)
	m.SetWindow(opt.Window)
	return m
}

// SetLimit changes the total download rate (0 = unlimited).
func (m *Manager) SetLimit(bps int64) {
	if bps <= 0 {
		m.limiter.SetLimit(rate.Inf)
	} else {
		m.limiter.SetLimit(rate.Limit(bps))
	}
}

// SetConcurrency changes the number of concurrent downloads.
func (m *Manager) SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	m.mu.Lock()
	m.concurrency = n
	m.mu.Unlock()
	m.poke()
}

// SetWindow changes the download window (nil = always open). A transfer in
// progress when the window closes runs to completion.
func (m *Manager) SetWindow(w *config.Window) {
	m.mu.Lock()
	m.window = w
	m.mu.Unlock()
	m.poke()
}

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) changed() {
	if m.opt.OnChange != nil {
		m.opt.OnChange()
	}
}

// Backoff returns the delay before the next attempt: base, then doubling, capped.
func Backoff(attempts int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempts && d < max; i++ {
		d *= 2
	}
	return min(d, max)
}

// SafeName turns a peer name into a safe folder name: never a separator, a
// control character, "." or "..".
func SafeName(name, id string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r == '/' || r == '\\' || r == 0:
			b.WriteRune('_')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if s == "" {
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		s = "peer-" + strings.NewReplacer("/", "_", "+", "-").Replace(short)
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func nano(t time.Time) int64 { return t.UnixNano() }

// Enqueue adds remote paths (files or folders, expanded recursively) of a
// peer. A file already present with the same size is marked done without a
// transfer; a refused path is counted in skipped.
func (m *Manager) Enqueue(ctx context.Context, peerID string, paths []string) (added, skipped int, err error) {
	peer, ok := m.opt.Peers(peerID)
	if !ok {
		return 0, 0, fmt.Errorf("unknown peer: %s", peerID)
	}
	folder := SafeName(peer.Name, peer.ID)
	root, err := os.OpenRoot(m.opt.Dir)
	if err != nil {
		return 0, 0, fmt.Errorf("downloads folder: %w", err)
	}
	defer root.Close()

	// Files received from the peer are written in batches, each in a short
	// transaction: never a transaction open across a network call. Otherwise
	// the single write connection would stay taken while the client waits for
	// the circle lock, held by a record update waiting for that very
	// connection (deadlock).
	const batch = 1000
	pending := make([]index.Entry, 0, batch)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		n, err := m.insertBatch(ctx, root, peer, folder, pending)
		added += n
		pending = pending[:0]
		return err
	}
	for _, p := range paths {
		cp, err := index.CleanPath(p)
		if err != nil {
			skipped++
			continue
		}
		err = m.opt.Remote.Tree(ctx, peer.IP, cp, func(e index.Entry) error {
			// The peer may lie: every received path is revalidated.
			if added+len(pending) >= maxEnqueue {
				return fmt.Errorf("more than %d files at once: select smaller folders", maxEnqueue)
			}
			rp, err := index.CleanPath(e.Path)
			if err != nil || rp == "" || e.IsDir || e.Size < 0 || len(rp) > maxPathLen {
				m.log.Warn("remote path refused", "peer", peer.Name, "path", e.Path)
				skipped++
				return nil
			}
			e.Path = rp
			pending = append(pending, e)
			if len(pending) >= batch {
				return flush()
			}
			return nil
		})
		if err == nil {
			err = flush()
		}
		if err != nil {
			return added, skipped, err
		}
	}
	m.poke()
	m.changed()
	return added, skipped, nil
}

// Caps on what a peer can make us queue at once.
const (
	maxEnqueue = 1_000_000
	maxPathLen = 4096
)

// insertBatch adds a batch of remote files in a single transaction. A file
// already present with the same size is marked done without a transfer, with
// no duplicate if the queue already knows it.
func (m *Manager) insertBatch(ctx context.Context, root *os.Root, peer Peer, folder string, entries []index.Entry) (int, error) {
	tx, err := m.opt.Store.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("queuing: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO queue
		(peer_id, peer_name, remote_path, dest_path, size, mtime, etag, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("queuing: %w", err)
	}
	defer stmt.Close()
	now := nano(m.now())
	added := 0
	for _, e := range entries {
		dest := folder + "/" + e.Path
		status := Queued
		if fi, err := root.Stat(dest); err == nil && fi.Mode().IsRegular() && fi.Size() == e.Size {
			var known int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue WHERE peer_id = ? AND remote_path = ?`, peer.ID, e.Path).Scan(&known); err != nil {
				return added, fmt.Errorf("queuing %s: %w", e.Path, err)
			}
			if known > 0 {
				continue
			}
			status = Done
		}
		res, err := stmt.ExecContext(ctx, peer.ID, peer.Name, e.Path, dest, e.Size, e.MTimeNano,
			etagOf(e.Size, e.MTimeNano), string(status), now, now)
		if err != nil {
			return added, fmt.Errorf("queuing %s: %w", e.Path, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("queuing: %w", err)
	}
	return added, nil
}

// etagOf reproduces the server's ETag (peerapi.ETag) from the index.
func etagOf(size, mtimeNano int64) string {
	return fmt.Sprintf(`"%d-%d"`, size, mtimeNano)
}

const itemCols = `id, peer_id, peer_name, remote_path, dest_path, size, mtime, etag, status, bytes_done,
	priority, attempts, next_attempt_at, last_error, created_at`

func scanItem(row interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var next, created int64
	var status string
	err := row.Scan(&it.ID, &it.PeerID, &it.PeerName, &it.RemotePath, &it.DestPath, &it.Size, &it.MTimeNano,
		&it.ETag, &status, &it.BytesDone, &it.Priority, &it.Attempts, &next, &it.LastError, &created)
	it.Status = Status(status)
	if next > 0 {
		it.NextAttempt = time.Unix(0, next)
	}
	it.CreatedAt = time.Unix(0, created)
	return it, err
}

// List returns the whole queue in processing order, with live progress of the
// active items.
func (m *Manager) List(ctx context.Context) ([]Item, error) {
	rows, err := m.opt.Store.R.QueryContext(ctx, "SELECT "+itemCols+` FROM queue
		ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'queued' THEN 1 WHEN 'waiting' THEN 2
		WHEN 'paused' THEN 3 WHEN 'error' THEN 4 ELSE 5 END, priority DESC, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("reading the queue: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("reading the queue: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the queue: %w", err)
	}
	m.withProgress(items)
	return items, nil
}

// Get returns an item.
func (m *Manager) Get(ctx context.Context, id int64) (Item, error) {
	it, err := scanItem(m.opt.Store.R.QueryRowContext(ctx, "SELECT "+itemCols+" FROM queue WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, fmt.Errorf("item %d not found", id)
	}
	return it, err
}

func (m *Manager) exec(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := m.opt.Store.W.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("updating the queue: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// stopJob interrupts a transfer in progress, if any.
func (m *Manager) stopJob(id int64, reason stopReason) bool {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if ok {
		j.reason.Store(int32(reason))
		j.cancel()
	}
	return ok
}

// Pause suspends an item (waiting or in progress).
func (m *Manager) Pause(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET status = 'paused', updated_at = ? WHERE id = ? AND status IN ('queued', 'waiting', 'active')`, nano(m.now()), id)
	if err == nil {
		m.stopJob(id, stopPause)
		m.changed()
	}
	return err
}

// Resume puts a paused item back in the queue.
func (m *Manager) Resume(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET status = 'queued', next_attempt_at = 0, updated_at = ? WHERE id = ? AND status = 'paused'`, nano(m.now()), id)
	m.poke()
	m.changed()
	return err
}

// Prioritize gives an item high priority.
func (m *Manager) Prioritize(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET priority = 1, updated_at = ? WHERE id = ?`, nano(m.now()), id)
	m.poke()
	m.changed()
	return err
}

// Retry restarts a failed item, with the attempt counter reset.
func (m *Manager) Retry(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET status = 'queued', attempts = 0, next_attempt_at = 0, last_error = '', updated_at = ?
		WHERE id = ? AND status = 'error'`, nano(m.now()), id)
	m.poke()
	m.changed()
	return err
}

// Remove removes an item; deletePart also deletes the partial file. A
// finished file is never deleted.
func (m *Manager) Remove(ctx context.Context, id int64, deletePart bool) error {
	it, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	reason := stopRemove
	if deletePart {
		reason = stopRemovePart
	}
	if _, err := m.exec(ctx, "DELETE FROM queue WHERE id = ?", id); err != nil {
		return err
	}
	// In progress: the transfer deletes the partial file itself when it stops.
	if !m.stopJob(id, reason) && deletePart {
		m.removePart(it)
	}
	m.changed()
	return nil
}

func (m *Manager) removePart(it Item) {
	root, err := os.OpenRoot(m.opt.Dir)
	if err != nil {
		return
	}
	defer root.Close()
	if err := root.Remove(partPath(it)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.log.Warn("deleting the partial file", "path", it.DestPath, "err", err)
	}
}

// Run drains the queue until ctx is cancelled. On start, items left "active"
// (abrupt stop) are queued again: resuming relies on the partial file. On
// stop, transfers in progress are interrupted cleanly and queued again.
func (m *Manager) Run(ctx context.Context) error {
	if _, err := m.exec(ctx, `UPDATE queue SET status = 'queued' WHERE status = 'active'`); err != nil {
		return err
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		next := m.schedule(ctx)
		if !next.IsZero() {
			timer.Reset(max(time.Until(next), 10*time.Millisecond))
		}
		select {
		case <-ctx.Done():
			m.wg.Wait()
			return nil
		case <-m.wake:
		case <-timer.C:
		}
	}
}

// schedule starts what can be started and returns the next deadline to watch
// (zero = nothing scheduled).
func (m *Manager) schedule(ctx context.Context) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.window != nil && !m.window.Contains(now) {
		return m.window.NextOpen(now)
	}
	slots := m.concurrency - len(m.jobs)
	if slots <= 0 {
		return time.Time{}
	}
	rows, err := m.opt.Store.R.QueryContext(ctx, "SELECT "+itemCols+` FROM queue
		WHERE status IN ('queued', 'waiting') AND next_attempt_at <= ?
		ORDER BY priority DESC, created_at, id LIMIT 500`, nano(now))
	if err != nil {
		m.log.Error("reading the queue", "err", err)
		return now.Add(5 * time.Second)
	}
	var start []Item
	for rows.Next() && len(start) < slots {
		it, err := scanItem(rows)
		if err != nil {
			continue
		}
		if m.busy[it.PeerID] || m.jobs[it.ID] != nil {
			continue
		}
		m.busy[it.PeerID] = true // reserved for the rest of this loop
		start = append(start, it)
	}
	rows.Close()
	for _, it := range start {
		jctx, cancel := context.WithCancel(ctx)
		j := &job{cancel: cancel, peer: it.PeerID}
		j.start.Store(time.Now().UnixNano())
		m.jobs[it.ID] = j
		m.wg.Go(func() { m.runJob(jctx, it, j) })
	}
	var nextAt int64
	_ = m.opt.Store.R.QueryRowContext(ctx, `SELECT COALESCE(MIN(next_attempt_at), 0) FROM queue
		WHERE status = 'waiting' AND next_attempt_at > ?`, nano(now)).Scan(&nextAt)
	if nextAt > 0 {
		return time.Unix(0, nextAt)
	}
	return time.Time{}
}

// errPermanent marks an error without retry (404, refused path…).
type errPermanent struct{ error }

func (e errPermanent) Unwrap() error { return e.error }

// errRetryAfter: the peer asked to wait (503), not counted as a failure.
type errRetryAfter struct {
	error
	after time.Duration
}

func classify(err error) error {
	var se statusError
	if errors.As(err, &se) {
		switch code := se.HTTPStatus(); {
		case code == http.StatusServiceUnavailable:
			return errRetryAfter{err, max(se.RetryDelay(), 5*time.Second)}
		case code == http.StatusNotFound || code == http.StatusBadRequest:
			return errPermanent{err}
		}
	}
	return err
}

func (m *Manager) runJob(ctx context.Context, it Item, j *job) {
	defer func() {
		m.mu.Lock()
		delete(m.jobs, it.ID)
		delete(m.busy, it.PeerID)
		m.mu.Unlock()
		j.cancel()
		m.poke()
		m.changed()
	}()
	bg := context.WithoutCancel(ctx) // final updates must go through even when stopping
	// Conditional: the item may have been paused or removed meanwhile.
	n, err := m.exec(bg, `UPDATE queue SET status = 'active', updated_at = ? WHERE id = ? AND status IN ('queued', 'waiting')`, nano(m.now()), it.ID)
	if err != nil {
		m.log.Error("queue", "err", err)
		return
	}
	if n == 0 {
		return
	}
	m.changed()
	log := m.log.With("id", it.ID, "peer", it.PeerName, "path", it.RemotePath)

	err = m.download(ctx, it, j)
	now := m.now()
	done := j.done.Load()
	switch reason := stopReason(j.reason.Load()); {
	case reason == stopPause:
		_, _ = m.exec(bg, `UPDATE queue SET bytes_done = ?, updated_at = ? WHERE id = ?`, done, nano(now), it.ID)
		log.Info("download paused", "bytes", done)
	case reason == stopRemove:
		log.Info("download removed")
	case reason == stopRemovePart:
		m.removePart(it)
		log.Info("download removed, partial file deleted")
	case err == nil:
		_, _ = m.exec(bg, `UPDATE queue SET status = 'done', bytes_done = size, last_error = '', updated_at = ? WHERE id = ?`, nano(now), it.ID)
		log.Info("download finished", "size", it.Size)
	case ctx.Err() != nil:
		// Node stopping: back to the queue, resumed on next start.
		_, _ = m.exec(bg, `UPDATE queue SET status = 'queued', bytes_done = ?, updated_at = ? WHERE id = ? AND status = 'active'`, done, nano(now), it.ID)
	default:
		var perm errPermanent
		var ra errRetryAfter
		switch err = classify(err); {
		case errors.As(err, &perm):
			_, _ = m.exec(bg, `UPDATE queue SET status = 'error', bytes_done = ?, last_error = ?, updated_at = ? WHERE id = ?`, done, err.Error(), nano(now), it.ID)
			log.Warn("download failed", "err", err)
		case errors.As(err, &ra):
			_, _ = m.exec(bg, `UPDATE queue SET status = 'waiting', bytes_done = ?, next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
				done, nano(now.Add(ra.after)), err.Error(), nano(now), it.ID)
			log.Info("peer busy, retry scheduled", "after", ra.after)
		default:
			wait := Backoff(it.Attempts+1, m.opt.BackoffBase, m.opt.BackoffMax)
			_, _ = m.exec(bg, `UPDATE queue SET status = 'waiting', attempts = attempts + 1, bytes_done = ?, next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
				done, nano(now.Add(wait)), err.Error(), nano(now), it.ID)
			log.Info("download interrupted, retry scheduled", "after", wait, "err", err)
		}
	}
}

// Summary counts items per status, with their total size.
type Summary struct {
	Count map[Status]int   `json:"count"`
	Bytes map[Status]int64 `json:"bytes"`
}

// Summarize returns the queue counters.
func (m *Manager) Summarize(ctx context.Context) (Summary, error) {
	s := Summary{Count: map[Status]int{}, Bytes: map[Status]int64{}}
	rows, err := m.opt.Store.R.QueryContext(ctx, "SELECT status, COUNT(*), COALESCE(SUM(size), 0) FROM queue GROUP BY status")
	if err != nil {
		return s, fmt.Errorf("reading the queue: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		var b int64
		if err := rows.Scan(&st, &n, &b); err != nil {
			return s, fmt.Errorf("reading the queue: %w", err)
		}
		s.Count[Status(st)], s.Bytes[Status(st)] = n, b
	}
	return s, rows.Err()
}

// ListPage returns a page of the queue, limited to some statuses (all if
// empty), in processing order; finished items newest first.
func (m *Manager) ListPage(ctx context.Context, statuses []Status, limit, offset int) ([]Item, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := "SELECT " + itemCols + " FROM queue"
	var args []any
	if len(statuses) > 0 {
		q += " WHERE status IN (?" + strings.Repeat(", ?", len(statuses)-1) + ")"
		for _, s := range statuses {
			args = append(args, string(s))
		}
	}
	q += ` ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'queued' THEN 1 WHEN 'waiting' THEN 2
		WHEN 'paused' THEN 3 WHEN 'error' THEN 4 ELSE 5 END,
		CASE WHEN status = 'done' THEN -updated_at ELSE 0 END, priority DESC, created_at, id LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := m.opt.Store.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("reading the queue: %w", err)
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("reading the queue: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	m.withProgress(items)
	return items, nil
}

// Active returns the transfers in progress with their progress.
func (m *Manager) Active(ctx context.Context) ([]Item, error) {
	return m.ListPage(ctx, []Status{Active}, 100, 0)
}

func (m *Manager) withProgress(items []Item) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range items {
		if j, ok := m.jobs[items[i].ID]; ok {
			items[i].BytesDone = j.done.Load()
			if el := time.Since(time.Unix(0, j.start.Load())).Seconds(); el > 0.5 {
				items[i].Rate = float64(j.done.Load()-j.startOffset.Load()) / el
			}
		}
	}
}

// ClearDone removes finished items from the queue (the files stay).
func (m *Manager) ClearDone(ctx context.Context) (int64, error) {
	n, err := m.exec(ctx, "DELETE FROM queue WHERE status = 'done'")
	m.changed()
	return n, err
}
