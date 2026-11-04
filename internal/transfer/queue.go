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

// Status est l'état d'un élément de la file.
type Status string

// États d'un élément.
const (
	Queued  Status = "queued"
	Active  Status = "active"
	Paused  Status = "paused"
	Waiting Status = "waiting" // pair indisponible, nouvel essai programmé
	Done    Status = "done"
	Failed  Status = "error"
)

// Item est un fichier distant à télécharger.
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
	Rate        float64   `json:"rate,omitempty"` // octets/s, éléments actifs seulement
}

// Peer est un pair joignable : son identité et son adresse de tunnel.
type Peer struct {
	ID   string
	Name string
	IP   netip.Addr
}

// Remote parle aux autres nœuds (implémenté par peerapi.Client).
type Remote interface {
	Tree(ctx context.Context, peer netip.Addr, dir string, fn func(index.Entry) error) error
	Download(ctx context.Context, peer netip.Addr, path string, offset int64, etag string) (*http.Response, error)
}

// statusError est une réponse HTTP non 2xx d'un pair (peerapi.StatusError).
type statusError interface {
	HTTPStatus() int
	RetryDelay() time.Duration
}

// Options configure un Manager.
type Options struct {
	Store       *store.Store
	Remote      Remote
	Peers       func(id string) (Peer, bool)
	Dir         string // dossier des téléchargements
	Concurrency int
	Limit       int64 // octets/s, 0 = illimité
	Window      *config.Window
	Logger      *slog.Logger
	OnChange    func() // appelé à chaque changement d'état (flux SSE de l'interface)

	// BackoffBase et BackoffMax : 30 s et 15 min par défaut, réduits dans les tests.
	BackoffBase, BackoffMax time.Duration
}

// Manager vide la file en arrière-plan.
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
	busy        map[string]bool // pairs en cours de transfert (1 au plus par pair)
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
	done        atomic.Int64 // octets présents dans le .part
	startOffset atomic.Int64 // octets déjà là au début de cette tentative
	start       atomic.Int64 // début de la tentative, nanosecondes Unix
	reason      atomic.Int32
}

// NewManager prépare la file ; Run la fait tourner.
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

// SetLimit change le débit total de téléchargement (0 = illimité).
func (m *Manager) SetLimit(bps int64) {
	if bps <= 0 {
		m.limiter.SetLimit(rate.Inf)
	} else {
		m.limiter.SetLimit(rate.Limit(bps))
	}
}

// SetConcurrency change le nombre de téléchargements simultanés.
func (m *Manager) SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	m.mu.Lock()
	m.concurrency = n
	m.mu.Unlock()
	m.poke()
}

// SetWindow change le créneau de téléchargement (nil = toujours ouvert).
// Un transfert en cours à la fermeture du créneau va à son terme.
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

// Backoff renvoie le délai avant la tentative suivante : base, puis ×2, plafonné.
func Backoff(attempts int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempts && d < max; i++ {
		d *= 2
	}
	return min(d, max)
}

// SafeName transforme un nom de pair en nom de dossier sûr : jamais de
// séparateur, de caractère de contrôle, de « . » ou « .. ».
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
		s = "pair-" + strings.NewReplacer("/", "_", "+", "-").Replace(short)
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func nano(t time.Time) int64 { return t.UnixNano() }

// Enqueue ajoute des chemins distants (fichiers ou dossiers, développés
// récursivement) d'un pair. Un fichier déjà présent avec la même taille est
// marqué terminé sans transfert ; un chemin refusé est compté dans skipped.
func (m *Manager) Enqueue(ctx context.Context, peerID string, paths []string) (added, skipped int, err error) {
	peer, ok := m.opt.Peers(peerID)
	if !ok {
		return 0, 0, fmt.Errorf("pair inconnu : %s", peerID)
	}
	folder := SafeName(peer.Name, peer.ID)
	root, err := os.OpenRoot(m.opt.Dir)
	if err != nil {
		return 0, 0, fmt.Errorf("dossier des téléchargements : %w", err)
	}
	defer root.Close()

	for _, p := range paths {
		cp, err := index.CleanPath(p)
		if err != nil {
			skipped++
			continue
		}
		tx, err := m.opt.Store.W.BeginTx(ctx, nil)
		if err != nil {
			return added, skipped, fmt.Errorf("ajout à la file : %w", err)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO queue
			(peer_id, peer_name, remote_path, dest_path, size, mtime, etag, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return added, skipped, fmt.Errorf("ajout à la file : %w", err)
		}
		now := nano(m.now())
		err = m.opt.Remote.Tree(ctx, peer.IP, cp, func(e index.Entry) error {
			// Le pair peut mentir : chaque chemin reçu est revalidé.
			rp, err := index.CleanPath(e.Path)
			if err != nil || rp == "" || e.IsDir || e.Size < 0 {
				m.log.Warn("chemin distant refusé", "peer", peer.Name, "path", e.Path)
				skipped++
				return nil
			}
			dest := folder + "/" + rp
			status := Queued
			if fi, err := root.Stat(dest); err == nil && fi.Mode().IsRegular() && fi.Size() == e.Size {
				// Déjà là : terminé sans transfert, sans doublon si la file le connaît déjà.
				var known int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue WHERE peer_id = ? AND remote_path = ?`, peer.ID, rp).Scan(&known); err != nil {
					return fmt.Errorf("ajout de %s : %w", rp, err)
				}
				if known > 0 {
					return nil
				}
				status = Done
			}
			res, err := stmt.ExecContext(ctx, peer.ID, peer.Name, rp, dest, e.Size, e.MTimeNano,
				etagOf(e.Size, e.MTimeNano), string(status), now, now)
			if err != nil {
				return fmt.Errorf("ajout de %s : %w", rp, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				added++
			}
			return nil
		})
		stmt.Close()
		if err != nil {
			tx.Rollback()
			return added, skipped, err
		}
		if err := tx.Commit(); err != nil {
			return added, skipped, fmt.Errorf("ajout à la file : %w", err)
		}
	}
	m.poke()
	m.changed()
	return added, skipped, nil
}

// etagOf reproduit l'ETag du serveur (peerapi.ETag) à partir de l'index.
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

// List renvoie toute la file dans l'ordre de traitement, avec la progression
// en direct des éléments actifs.
func (m *Manager) List(ctx context.Context) ([]Item, error) {
	rows, err := m.opt.Store.R.QueryContext(ctx, "SELECT "+itemCols+` FROM queue
		ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'queued' THEN 1 WHEN 'waiting' THEN 2
		WHEN 'paused' THEN 3 WHEN 'error' THEN 4 ELSE 5 END, priority DESC, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("lecture de la file : %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("lecture de la file : %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lecture de la file : %w", err)
	}
	m.mu.Lock()
	for i := range items {
		if j, ok := m.jobs[items[i].ID]; ok {
			items[i].BytesDone = j.done.Load()
			if el := time.Since(time.Unix(0, j.start.Load())).Seconds(); el > 0.5 {
				items[i].Rate = float64(j.done.Load()-j.startOffset.Load()) / el
			}
		}
	}
	m.mu.Unlock()
	return items, nil
}

// Get renvoie un élément.
func (m *Manager) Get(ctx context.Context, id int64) (Item, error) {
	it, err := scanItem(m.opt.Store.R.QueryRowContext(ctx, "SELECT "+itemCols+" FROM queue WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, fmt.Errorf("élément %d introuvable", id)
	}
	return it, err
}

func (m *Manager) exec(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := m.opt.Store.W.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("mise à jour de la file : %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// stopJob interrompt un transfert en cours, s'il y en a un.
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

// Pause suspend un élément (en attente ou en cours).
func (m *Manager) Pause(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET status = 'paused', updated_at = ? WHERE id = ? AND status IN ('queued', 'waiting', 'active')`, nano(m.now()), id)
	if err == nil {
		m.stopJob(id, stopPause)
		m.changed()
	}
	return err
}

// Resume remet un élément suspendu dans la file.
func (m *Manager) Resume(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET status = 'queued', next_attempt_at = 0, updated_at = ? WHERE id = ? AND status = 'paused'`, nano(m.now()), id)
	m.poke()
	m.changed()
	return err
}

// Prioritize passe un élément en priorité haute.
func (m *Manager) Prioritize(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET priority = 1, updated_at = ? WHERE id = ?`, nano(m.now()), id)
	m.poke()
	m.changed()
	return err
}

// Retry relance un élément en erreur, compteur de tentatives remis à zéro.
func (m *Manager) Retry(ctx context.Context, id int64) error {
	_, err := m.exec(ctx, `UPDATE queue SET status = 'queued', attempts = 0, next_attempt_at = 0, last_error = '', updated_at = ?
		WHERE id = ? AND status = 'error'`, nano(m.now()), id)
	m.poke()
	m.changed()
	return err
}

// Remove retire un élément ; deletePart supprime aussi le fichier partiel.
// Le fichier terminé, lui, n'est jamais supprimé.
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
	// En cours : le transfert supprime lui-même le .part en s'arrêtant.
	if !m.stopJob(id, reason) && deletePart {
		m.removePart(it.DestPath)
	}
	m.changed()
	return nil
}

func (m *Manager) removePart(dest string) {
	root, err := os.OpenRoot(m.opt.Dir)
	if err != nil {
		return
	}
	defer root.Close()
	if err := root.Remove(dest + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.log.Warn("suppression du fichier partiel", "path", dest, "err", err)
	}
}

// Run vide la file jusqu'à l'annulation de ctx. Au démarrage, les éléments
// restés « actifs » (arrêt brutal) repassent en file : la reprise se fait par
// le fichier .part. À l'arrêt, les transferts en cours sont interrompus
// proprement et repassent en file.
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

// schedule démarre ce qui peut l'être et renvoie la prochaine échéance à
// surveiller (zéro = rien de programmé).
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
		m.log.Error("lecture de la file", "err", err)
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
		m.busy[it.PeerID] = true // réservé pour la suite de cette boucle
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

// errPermanent marque une erreur sans nouvel essai (404, chemin refusé…).
type errPermanent struct{ error }

func (e errPermanent) Unwrap() error { return e.error }

// errRetryAfter : le pair a demandé d'attendre (503), sans compter d'échec.
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
	bg := context.WithoutCancel(ctx) // les mises à jour finales doivent passer même à l'arrêt
	// Conditionnel : l'élément a pu être suspendu ou retiré entre-temps.
	n, err := m.exec(bg, `UPDATE queue SET status = 'active', updated_at = ? WHERE id = ? AND status IN ('queued', 'waiting')`, nano(m.now()), it.ID)
	if err != nil {
		m.log.Error("file", "err", err)
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
		log.Info("téléchargement suspendu", "bytes", done)
	case reason == stopRemove:
		log.Info("téléchargement retiré")
	case reason == stopRemovePart:
		m.removePart(it.DestPath)
		log.Info("téléchargement retiré, fichier partiel supprimé")
	case err == nil:
		_, _ = m.exec(bg, `UPDATE queue SET status = 'done', bytes_done = size, last_error = '', updated_at = ? WHERE id = ?`, nano(now), it.ID)
		log.Info("téléchargement terminé", "size", it.Size)
	case ctx.Err() != nil:
		// Arrêt du nœud : retour en file, reprise au prochain démarrage.
		_, _ = m.exec(bg, `UPDATE queue SET status = 'queued', bytes_done = ?, updated_at = ? WHERE id = ? AND status = 'active'`, done, nano(now), it.ID)
	default:
		var perm errPermanent
		var ra errRetryAfter
		switch err = classify(err); {
		case errors.As(err, &perm):
			_, _ = m.exec(bg, `UPDATE queue SET status = 'error', bytes_done = ?, last_error = ?, updated_at = ? WHERE id = ?`, done, err.Error(), nano(now), it.ID)
			log.Warn("téléchargement en erreur", "err", err)
		case errors.As(err, &ra):
			_, _ = m.exec(bg, `UPDATE queue SET status = 'waiting', bytes_done = ?, next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
				done, nano(now.Add(ra.after)), err.Error(), nano(now), it.ID)
			log.Info("pair occupé, nouvel essai programmé", "after", ra.after)
		default:
			wait := Backoff(it.Attempts+1, m.opt.BackoffBase, m.opt.BackoffMax)
			_, _ = m.exec(bg, `UPDATE queue SET status = 'waiting', attempts = attempts + 1, bytes_done = ?, next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
				done, nano(now.Add(wait)), err.Error(), nano(now), it.ID)
			log.Info("téléchargement interrompu, nouvel essai programmé", "after", wait, "err", err)
		}
	}
}
