package index

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DB-CL/cercle/internal/store"
)

// Entry est un fichier ou un dossier du partage.
type Entry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size"`     // taille cumulée pour un dossier
	MTimeNano int64  `json:"mtime_ns"` // nanosecondes Unix
	FileCount int64  `json:"file_count,omitempty"`
}

// ModTime renvoie la date de modification.
func (e Entry) ModTime() time.Time { return time.Unix(0, e.MTimeNano) }

// Stats résume un scan.
type Stats struct {
	Files, Dirs, Skipped int64
	Size                 int64
	Duration             time.Duration
}

// Index est l'index du dossier partagé.
type Index struct {
	st       *store.Store
	dir      string
	excludes []string
	log      *slog.Logger
	scanMu   sync.Mutex // un seul scan à la fois
	trigger  chan struct{}
}

// New prépare l'index du dossier dir. excludes sont des motifs glob (path.Match)
// testés sur le nom et sur le chemin relatif.
func New(st *store.Store, dir string, excludes []string, log *slog.Logger) *Index {
	if log == nil {
		log = slog.Default()
	}
	return &Index{st: st, dir: dir, excludes: excludes, log: log.With("module", "index"), trigger: make(chan struct{}, 1)}
}

// Run lance un scan tout de suite, puis toutes les interval, et à chaque
// TriggerScan, jusqu'à l'annulation de ctx.
func (ix *Index) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		st, err := ix.Scan(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			ix.log.Error("scan du partage en échec", "err", err)
		default:
			ix.log.Info("scan du partage terminé", "files", st.Files, "dirs", st.Dirs, "size", st.Size, "skipped", st.Skipped, "duration", st.Duration.Round(time.Millisecond).String())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-ix.trigger:
		}
	}
}

// TriggerScan demande un scan sans attendre.
func (ix *Index) TriggerScan() {
	select {
	case ix.trigger <- struct{}{}:
	default:
	}
}

// excluded dit si une entrée est exclue du partage.
func (ix *Index) excluded(rel, name string) bool {
	switch {
	case strings.HasPrefix(name, "."), name == "@eaDir", name == "#recycle", name == "$RECYCLE.BIN":
		return true // .Trash-* commence aussi par un point
	}
	for _, pat := range ix.excludes {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// dirAcc accumule la taille et le nombre de fichiers d'un dossier en cours de parcours.
type dirAcc struct {
	path  string
	mtime int64
	size  int64
	count int64
}

const batchRows = 2000

// Scan parcourt le partage et met l'index à jour. Il ne lit jamais le contenu
// des fichiers : seulement les métadonnées (taille, date). Une seule goroutine,
// pour ne pas saturer les disques du NAS.
func (ix *Index) Scan(ctx context.Context) (Stats, error) {
	ix.scanMu.Lock()
	defer ix.scanMu.Unlock()
	start := time.Now()
	var st Stats

	root, err := os.OpenRoot(ix.dir)
	if err != nil {
		return st, fmt.Errorf("ouverture du partage %s : %w", ix.dir, err)
	}
	defer root.Close()
	fsys := root.FS()
	rootInfo, err := fs.Stat(fsys, ".")
	if err != nil {
		return st, fmt.Errorf("lecture du partage %s : %w", ix.dir, err)
	}

	var gen int64
	if err := ix.st.W.QueryRowContext(ctx, "SELECT COALESCE(MAX(scan_gen), 0) + 1 FROM files").Scan(&gen); err != nil {
		return st, fmt.Errorf("génération de scan : %w", err)
	}

	w := &batchWriter{ctx: ctx, db: ix.st.W, gen: gen}
	defer w.abort()

	stack := []*dirAcc{{path: "", mtime: rootInfo.ModTime().UnixNano()}}
	// closeDir écrit la ligne du dossier au sommet de la pile et remonte ses totaux.
	closeDir := func() error {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if len(stack) > 0 {
			parent := stack[len(stack)-1]
			parent.size += top.size
			parent.count += top.count
		}
		st.Dirs++
		return w.put(Entry{Path: top.path, IsDir: true, Size: top.size, MTimeNano: top.mtime, FileCount: top.count})
	}

	walkErr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == "." {
			return err // racine illisible : abandon
		}
		if err != nil {
			ix.log.Warn("entrée illisible ignorée", "path", p, "err", err)
			st.Skipped++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ix.excluded(p, d.Name()) {
			st.Skipped++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		parent := parentOf(p)
		for stack[len(stack)-1].path != parent {
			if err := closeDir(); err != nil {
				return err
			}
		}

		var info fs.FileInfo
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			// os.Root ne suit un lien que s'il reste dans le partage ; sinon erreur.
			info, err = fs.Stat(fsys, p)
			if err != nil || !info.Mode().IsRegular() {
				// Lien sortant, cassé, ou vers un dossier (risque de boucle) : ignoré.
				st.Skipped++
				return nil
			}
		case d.IsDir():
			info, err = d.Info()
			if err != nil {
				st.Skipped++
				return fs.SkipDir
			}
			stack = append(stack, &dirAcc{path: p, mtime: info.ModTime().UnixNano()})
			return nil
		case d.Type().IsRegular():
			info, err = d.Info()
			if err != nil {
				st.Skipped++
				return nil
			}
		default:
			st.Skipped++ // socket, périphérique, tube
			return nil
		}
		top := stack[len(stack)-1]
		top.size += info.Size()
		top.count++
		st.Files++
		return w.put(Entry{Path: p, Size: info.Size(), MTimeNano: info.ModTime().UnixNano()})
	})
	if walkErr != nil {
		return st, fmt.Errorf("parcours du partage : %w", walkErr)
	}
	for len(stack) > 0 {
		if err := closeDir(); err != nil {
			return st, fmt.Errorf("parcours du partage : %w", err)
		}
	}
	st.Dirs-- // la racine n'est pas un dossier du partage
	if err := w.finish(); err != nil {
		return st, err
	}
	st.Size = 0
	_ = ix.st.R.QueryRowContext(ctx, "SELECT size FROM files WHERE path = ''").Scan(&st.Size)
	st.Duration = time.Since(start)
	if err := ix.st.SetSetting(ctx, "index.last_scan", strconv.FormatInt(time.Now().UnixNano(), 10)); err != nil {
		return st, err
	}
	return st, nil
}

// batchWriter écrit l'index par transactions de batchRows lignes, puis
// supprime ce que le scan n'a pas revu.
type batchWriter struct {
	ctx  context.Context
	db   *sql.DB
	gen  int64
	tx   *sql.Tx
	stmt *sql.Stmt
	n    int
}

func (w *batchWriter) put(e Entry) error {
	if w.tx == nil {
		tx, err := w.db.BeginTx(w.ctx, nil)
		if err != nil {
			return fmt.Errorf("écriture de l'index : %w", err)
		}
		stmt, err := tx.PrepareContext(w.ctx, `INSERT INTO files (path, parent, name, is_dir, size, mtime, file_count, scan_gen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (path) DO UPDATE SET is_dir = excluded.is_dir, size = excluded.size,
				mtime = excluded.mtime, file_count = excluded.file_count, scan_gen = excluded.scan_gen`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("écriture de l'index : %w", err)
		}
		w.tx, w.stmt = tx, stmt
	}
	name := path.Base(e.Path)
	if e.Path == "" {
		name = ""
	}
	if _, err := w.stmt.ExecContext(w.ctx, e.Path, parentOf(e.Path), name, e.IsDir, e.Size, e.MTimeNano, e.FileCount, w.gen); err != nil {
		return fmt.Errorf("écriture de l'index (%s) : %w", e.Path, err)
	}
	w.n++
	if w.n >= batchRows {
		return w.commit()
	}
	return nil
}

func (w *batchWriter) commit() error {
	if w.tx == nil {
		return nil
	}
	w.stmt.Close()
	err := w.tx.Commit()
	w.tx, w.stmt, w.n = nil, nil, 0
	if err != nil {
		return fmt.Errorf("écriture de l'index : %w", err)
	}
	return nil
}

func (w *batchWriter) finish() error {
	if err := w.commit(); err != nil {
		return err
	}
	if _, err := w.db.ExecContext(w.ctx, "DELETE FROM files WHERE scan_gen < ?", w.gen); err != nil {
		return fmt.Errorf("purge de l'index : %w", err)
	}
	return nil
}

func (w *batchWriter) abort() {
	if w.tx != nil {
		w.stmt.Close()
		w.tx.Rollback()
		w.tx = nil
	}
}

const entryCols = "path, name, is_dir, size, mtime, file_count"

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	err := row.Scan(&e.Path, &e.Name, &e.IsDir, &e.Size, &e.MTimeNano, &e.FileCount)
	return e, err
}

// Lookup renvoie l'entrée d'un chemin normalisé ("" = racine).
func (ix *Index) Lookup(ctx context.Context, p string) (Entry, error) {
	e, err := scanEntry(ix.st.R.QueryRowContext(ctx, "SELECT "+entryCols+" FROM files WHERE path = ?", p))
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("lecture de l'index : %w", err)
	}
	return e, nil
}

// PageSize est la taille d'une page de List.
const PageSize = 500

// List renvoie une page du contenu d'un dossier, dossiers d'abord puis par
// nom. next est vide sur la dernière page.
func (ix *Index) List(ctx context.Context, dir, cursor string, limit int) (entries []Entry, next string, err error) {
	if limit <= 0 || limit > PageSize {
		limit = PageSize
	}
	d, err := ix.Lookup(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	if !d.IsDir {
		return nil, "", fmt.Errorf("%s n'est pas un dossier : %w", dir, ErrNotFound)
	}
	q := "SELECT " + entryCols + " FROM files WHERE parent = ? AND path <> ''"
	args := []any{dir}
	if cursor != "" {
		cIsDir, cName, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		q += ` AND (is_dir < ? OR (is_dir = ? AND (name > ? COLLATE NOCASE
			OR (name = ? COLLATE NOCASE AND name > ? COLLATE BINARY))))`
		args = append(args, cIsDir, cIsDir, cName, cName, cName)
	}
	q += " ORDER BY is_dir DESC, name COLLATE NOCASE, name LIMIT ?"
	args = append(args, limit+1)
	rows, err := ix.st.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("lecture de l'index : %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, "", fmt.Errorf("lecture de l'index : %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("lecture de l'index : %w", err)
	}
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[limit-1]
		next = encodeCursor(last.IsDir, last.Name)
	}
	return entries, next, nil
}

func encodeCursor(isDir bool, name string) string {
	prefix := "f:"
	if isDir {
		prefix = "d:"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(prefix + name))
}

func decodeCursor(c string) (isDir bool, name string, err error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) < 2 || (b[0] != 'd' && b[0] != 'f') || b[1] != ':' {
		return false, "", fmt.Errorf("curseur invalide : %w", ErrBadPath)
	}
	return b[0] == 'd', string(b[2:]), nil
}

// Tree appelle fn pour chaque fichier sous p (p lui-même si c'est un fichier),
// dans l'ordre des chemins.
func (ix *Index) Tree(ctx context.Context, p string, fn func(Entry) error) error {
	e, err := ix.Lookup(ctx, p)
	if err != nil {
		return err
	}
	if !e.IsDir {
		return fn(e)
	}
	q := "SELECT " + entryCols + " FROM files WHERE is_dir = 0"
	var args []any
	if p != "" {
		// Tous les chemins qui commencent par "p/" : "0" suit "/" en ASCII.
		q += " AND path > ? AND path < ?"
		args = append(args, p+"/", p+"0")
	}
	q += " ORDER BY path"
	rows, err := ix.st.R.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("lecture de l'index : %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return fmt.Errorf("lecture de l'index : %w", err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Totals renvoie la taille et le nombre de fichiers partagés, et la date du
// dernier scan (nil si aucun).
func (ix *Index) Totals(ctx context.Context) (size, files int64, lastScan *time.Time) {
	_ = ix.st.R.QueryRowContext(ctx, "SELECT size, file_count FROM files WHERE path = ''").Scan(&size, &files)
	if v, ok, err := ix.st.Setting(ctx, "index.last_scan"); ok && err == nil {
		if ns, err := strconv.ParseInt(v, 10, 64); err == nil {
			t := time.Unix(0, ns)
			lastScan = &t
		}
	}
	return size, files, lastScan
}

// Open ouvre un fichier indexé pour le servir. Le chemin doit figurer dans
// l'index (un fichier exclu ou caché est introuvable, même si l'on devine son
// nom), et os.Root interdit toute sortie du partage, liens symboliques compris.
func (ix *Index) Open(ctx context.Context, p string) (*os.File, fs.FileInfo, error) {
	e, err := ix.Lookup(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if e.IsDir {
		return nil, nil, fmt.Errorf("%s est un dossier : %w", p, ErrNotFound)
	}
	root, err := os.OpenRoot(ix.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("ouverture du partage : %w", err)
	}
	defer root.Close() // le fichier ouvert reste valide après la fermeture de Root
	f, err := root.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w : %v", ErrBadPath, err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}
