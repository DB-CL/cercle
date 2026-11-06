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

// Entry is a file or folder of the share.
type Entry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size"`     // cumulated size for a folder
	MTimeNano int64  `json:"mtime_ns"` // Unix nanoseconds
	FileCount int64  `json:"file_count,omitempty"`
}

// ModTime returns the modification time.
func (e Entry) ModTime() time.Time { return time.Unix(0, e.MTimeNano) }

// Stats summarizes a scan.
type Stats struct {
	Files, Dirs, Skipped int64
	Size                 int64
	Duration             time.Duration
}

// Index is the shared folder's index.
type Index struct {
	st       *store.Store
	dir      string
	excludes []string
	log      *slog.Logger
	scanMu   sync.Mutex // one scan at a time
	trigger  chan struct{}

	// OnScan is called after every successful scan (interface notification).
	OnScan func()
}

// New prepares the index of folder dir. excludes are glob patterns
// (path.Match) tested against the name and the relative path.
func New(st *store.Store, dir string, excludes []string, log *slog.Logger) *Index {
	if log == nil {
		log = slog.Default()
	}
	return &Index{st: st, dir: dir, excludes: excludes, log: log.With("module", "index"), trigger: make(chan struct{}, 1)}
}

// Run scans right away, then every interval and on every TriggerScan, until
// ctx is cancelled.
func (ix *Index) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		st, err := ix.Scan(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			ix.log.Error("share scan failed", "err", err)
		default:
			if ix.OnScan != nil {
				ix.OnScan()
			}
			ix.log.Info("share scan finished", "files", st.Files, "dirs", st.Dirs, "size", st.Size, "skipped", st.Skipped, "duration", st.Duration.Round(time.Millisecond).String())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-ix.trigger:
		}
	}
}

// TriggerScan requests a scan without waiting.
func (ix *Index) TriggerScan() {
	select {
	case ix.trigger <- struct{}{}:
	default:
	}
}

// excluded reports whether an entry is excluded from the share.
func (ix *Index) excluded(rel, name string) bool {
	switch {
	case strings.HasPrefix(name, "."), name == "@eaDir", name == "#recycle", name == "$RECYCLE.BIN":
		return true // .Trash-* also starts with a dot
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

// dirAcc accumulates the size and file count of the folder being walked.
type dirAcc struct {
	path  string
	mtime int64
	size  int64
	count int64
}

const batchRows = 2000

// Scan walks the share and updates the index. It never reads file contents,
// only metadata (size, date). A single goroutine, so as not to saturate the
// NAS disks.
func (ix *Index) Scan(ctx context.Context) (Stats, error) {
	ix.scanMu.Lock()
	defer ix.scanMu.Unlock()
	start := time.Now()
	var st Stats

	root, err := os.OpenRoot(ix.dir)
	if err != nil {
		return st, fmt.Errorf("opening the share %s: %w", ix.dir, err)
	}
	defer root.Close()
	fsys := root.FS()
	rootInfo, err := fs.Stat(fsys, ".")
	if err != nil {
		return st, fmt.Errorf("reading the share %s: %w", ix.dir, err)
	}

	sel, err := ix.Selection(ctx)
	if err != nil {
		return st, err
	}

	var gen int64
	if err := ix.st.W.QueryRowContext(ctx, "SELECT COALESCE(MAX(scan_gen), 0) + 1 FROM files").Scan(&gen); err != nil {
		return st, fmt.Errorf("scan generation: %w", err)
	}

	w := &batchWriter{ctx: ctx, db: ix.st.W, gen: gen}
	defer w.abort()

	stack := []*dirAcc{{path: "", mtime: rootInfo.ModTime().UnixNano()}}
	// closeDir writes the row of the folder on top of the stack and propagates its totals.
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
			return err // unreadable root: give up
		}
		if err != nil {
			ix.log.Warn("unreadable entry skipped", "path", p, "err", err)
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
		if parent == "" && !sel.Shares(d.Name()) {
			// First-level entry not ticked in the settings: neither indexed nor served.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		for stack[len(stack)-1].path != parent {
			if err := closeDir(); err != nil {
				return err
			}
		}

		var info fs.FileInfo
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			// os.Root only follows a link that stays inside the share; otherwise it errors.
			info, err = fs.Stat(fsys, p)
			if err != nil || !info.Mode().IsRegular() {
				// Escaping, broken, or pointing to a folder (loop risk): skipped.
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
			st.Skipped++ // socket, device, pipe
			return nil
		}
		top := stack[len(stack)-1]
		top.size += info.Size()
		top.count++
		st.Files++
		return w.put(Entry{Path: p, Size: info.Size(), MTimeNano: info.ModTime().UnixNano()})
	})
	if walkErr != nil {
		return st, fmt.Errorf("walking the share: %w", walkErr)
	}
	for len(stack) > 0 {
		if err := closeDir(); err != nil {
			return st, fmt.Errorf("walking the share: %w", err)
		}
	}
	st.Dirs-- // the root is not a folder of the share
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

// batchWriter writes the index in transactions of batchRows rows, then
// deletes what the scan did not see again.
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
			return fmt.Errorf("writing the index: %w", err)
		}
		stmt, err := tx.PrepareContext(w.ctx, `INSERT INTO files (path, parent, name, is_dir, size, mtime, file_count, scan_gen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (path) DO UPDATE SET is_dir = excluded.is_dir, size = excluded.size,
				mtime = excluded.mtime, file_count = excluded.file_count, scan_gen = excluded.scan_gen`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("writing the index: %w", err)
		}
		w.tx, w.stmt = tx, stmt
	}
	name := path.Base(e.Path)
	if e.Path == "" {
		name = ""
	}
	if _, err := w.stmt.ExecContext(w.ctx, e.Path, parentOf(e.Path), name, e.IsDir, e.Size, e.MTimeNano, e.FileCount, w.gen); err != nil {
		return fmt.Errorf("writing the index (%s): %w", e.Path, err)
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
		return fmt.Errorf("writing the index: %w", err)
	}
	return nil
}

func (w *batchWriter) finish() error {
	if err := w.commit(); err != nil {
		return err
	}
	if _, err := w.db.ExecContext(w.ctx, "DELETE FROM files WHERE scan_gen < ?", w.gen); err != nil {
		return fmt.Errorf("purging the index: %w", err)
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

// Lookup returns the entry of a normalized path ("" = root).
func (ix *Index) Lookup(ctx context.Context, p string) (Entry, error) {
	e, err := scanEntry(ix.st.R.QueryRowContext(ctx, "SELECT "+entryCols+" FROM files WHERE path = ?", p))
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("reading the index: %w", err)
	}
	return e, nil
}

// PageSize is the size of a List page.
const PageSize = 500

// List returns a page of a folder's contents, folders first then by name.
// next is empty on the last page.
func (ix *Index) List(ctx context.Context, dir, cursor string, limit int) (entries []Entry, next string, err error) {
	if limit <= 0 || limit > PageSize {
		limit = PageSize
	}
	d, err := ix.Lookup(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	if !d.IsDir {
		return nil, "", fmt.Errorf("%s is not a folder: %w", dir, ErrNotFound)
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
		return nil, "", fmt.Errorf("reading the index: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, "", fmt.Errorf("reading the index: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("reading the index: %w", err)
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
		return false, "", fmt.Errorf("invalid cursor: %w", ErrBadPath)
	}
	return b[0] == 'd', string(b[2:]), nil
}

// Tree calls fn for every file under p (p itself if it is a file), in path
// order.
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
		// Every path starting with "p/": "0" follows "/" in ASCII.
		q += " AND path > ? AND path < ?"
		args = append(args, p+"/", p+"0")
	}
	q += " ORDER BY path"
	rows, err := ix.st.R.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("reading the index: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return fmt.Errorf("reading the index: %w", err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Totals returns the shared size and file count, and the time of the last
// scan (nil if none).
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

// Open opens an indexed file to serve it. The path must be in the index (an
// excluded or hidden file is not found, even if its name is guessed), and
// os.Root prevents escaping the share, symlinks included.
func (ix *Index) Open(ctx context.Context, p string) (*os.File, fs.FileInfo, error) {
	e, err := ix.Lookup(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if e.IsDir {
		return nil, nil, fmt.Errorf("%s is a folder: %w", p, ErrNotFound)
	}
	root, err := os.OpenRoot(ix.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the share: %w", err)
	}
	defer root.Close() // the opened file stays valid after Root is closed
	f, err := root.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrBadPath, err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}
