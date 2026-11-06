package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigrateAndSettings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cercle.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.R.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	if _, ok, err := s.Setting(ctx, "upload_limit"); ok || err != nil {
		t.Fatalf("expected no setting: %v, %v", ok, err)
	}
	if err := s.SetSetting(ctx, "upload_limit", "10M"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "upload_limit", "20M"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.R.ExecContext(ctx, "INSERT INTO settings VALUES ('x', 'y')"); err == nil {
		t.Error("the read pool accepts a write")
	}
	s.Close()

	// Reopening: migrations already applied, setting kept.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, ok, err := s.Setting(ctx, "upload_limit")
	if !ok || err != nil || v != "20M" {
		t.Fatalf("setting read back = %q, %v, %v", v, ok, err)
	}
	var ver int
	if err := s.R.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil || ver < 1 {
		t.Fatalf("user_version = %d, %v", ver, err)
	}
}
