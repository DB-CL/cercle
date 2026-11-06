// Package store opens the node's SQLite database and applies its migrations.
//
// Two pools on the same file: a single write connection (writes are
// serialized, never SQLITE_BUSY between ourselves), and a few parallel read
// connections thanks to WAL mode.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // "sqlite" driver, no cgo
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store holds both pools.
type Store struct {
	W *sql.DB // writes, one connection
	R *sql.DB // reads
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// Open opens (or creates) the database and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	if err := migrate(ctx, w); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("opening %s read-only: %w", path, err)
	}
	r.SetMaxOpenConns(4)
	return &Store{W: w, R: r}, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	return errors.Join(s.R.Close(), s.W.Close())
}

// migrate applies, in order, the NNN_name.sql files whose number is above
// PRAGMA user_version.
func migrate(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return fmt.Errorf("badly named migration: %s", base)
		}
		if v <= current {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(v)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
	}
	return nil
}

// Setting reads a stored setting. ok is false if it was never changed.
func (s *Store) Setting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.R.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading setting %s: %w", key, err)
	}
	return value, true, nil
}

// SetSetting stores a setting; it then takes precedence over the environment.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.W.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		key, value)
	if err != nil {
		return fmt.Errorf("writing setting %s: %w", key, err)
	}
	return nil
}
