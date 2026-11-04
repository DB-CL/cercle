// Package store ouvre la base SQLite du nœud et applique ses migrations.
//
// Deux pools sur le même fichier : une seule connexion d'écriture (les
// écritures sont sérialisées, jamais de SQLITE_BUSY entre nous), et quelques
// connexions de lecture en parallèle grâce au mode WAL.
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

	_ "modernc.org/sqlite" // pilote "sqlite", sans cgo
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store regroupe les deux pools.
type Store struct {
	W *sql.DB // écritures, une connexion
	R *sql.DB // lectures
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

// Open ouvre (ou crée) la base et la met à jour.
func Open(ctx context.Context, path string) (*Store, error) {
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("ouverture de %s : %w", path, err)
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
		return nil, fmt.Errorf("ouverture de %s en lecture : %w", path, err)
	}
	r.SetMaxOpenConns(4)
	return &Store{W: w, R: r}, nil
}

// Close ferme les deux pools.
func (s *Store) Close() error {
	return errors.Join(s.R.Close(), s.W.Close())
}

// migrate applique dans l'ordre les fichiers NNN_nom.sql dont le numéro
// dépasse PRAGMA user_version.
func migrate(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("lecture de la version du schéma : %w", err)
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
			return fmt.Errorf("migration mal nommée : %s", base)
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
			return fmt.Errorf("migration %s : %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s : %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(v)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s : %w", base, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s : %w", base, err)
		}
	}
	return nil
}

// Setting lit un réglage stocké. ok est faux s'il n'a jamais été modifié.
func (s *Store) Setting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.R.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lecture du réglage %s : %w", key, err)
	}
	return value, true, nil
}

// SetSetting enregistre un réglage ; il primera désormais sur l'environnement.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.W.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		key, value)
	if err != nil {
		return fmt.Errorf("écriture du réglage %s : %w", key, err)
	}
	return nil
}
