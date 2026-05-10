package migrate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/migrate"
)

func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	return db
}

func writeMigration(t *testing.T, dir, name, sql string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestRunner_AppliesPendingInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMigration(t, dir, "001_a.sql", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	writeMigration(t, dir, "002_b.sql", "CREATE TABLE b (id INTEGER PRIMARY KEY);")
	db := newDB(t)
	r, err := migrate.New(migrate.Config{DB: db, Dir: dir})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	applied, err := r.Apply(context.Background())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 2 || applied[0].Version != 1 || applied[1].Version != 2 {
		t.Fatalf("applied=%+v", applied)
	}
	var rows []migrate.SchemaMigration
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("schema_migrations rows=%d", len(rows))
	}
}

func TestRunner_IsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMigration(t, dir, "001_a.sql", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	db := newDB(t)
	r, _ := migrate.New(migrate.Config{DB: db, Dir: dir})
	if _, err := r.Apply(context.Background()); err != nil {
		t.Fatalf("apply1: %v", err)
	}
	pending, err := r.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after apply: %+v", pending)
	}
	if _, err := r.Apply(context.Background()); err != nil {
		t.Fatalf("apply2: %v", err)
	}
}

func TestRunner_DryRunSkipsApply(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMigration(t, dir, "001_a.sql", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	db := newDB(t)
	r, _ := migrate.New(migrate.Config{DB: db, Dir: dir})
	pending, err := r.DryRun(context.Background())
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	// Confirm DryRun did NOT apply the migration.
	if err := db.Exec("INSERT INTO a (id) VALUES (1);").Error; err == nil {
		t.Fatalf("dryrun should not have created table 'a'")
	}
}

func TestRunner_RejectsBadFilename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMigration(t, dir, "no-prefix.sql", "SELECT 1;")
	db := newDB(t)
	r, _ := migrate.New(migrate.Config{DB: db, Dir: dir})
	if _, err := r.Apply(context.Background()); err == nil {
		t.Fatalf("expected error for bad filename")
	}
}

func TestRunner_RejectsDuplicateVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMigration(t, dir, "001_a.sql", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	writeMigration(t, dir, "001_b.sql", "CREATE TABLE b (id INTEGER PRIMARY KEY);")
	db := newDB(t)
	r, _ := migrate.New(migrate.Config{DB: db, Dir: dir})
	if _, err := r.Apply(context.Background()); err == nil {
		t.Fatalf("expected duplicate version error")
	}
}

func TestRunner_RollsBackOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMigration(t, dir, "001_ok.sql", "CREATE TABLE ok (id INTEGER PRIMARY KEY);")
	writeMigration(t, dir, "002_bad.sql", "NOT VALID SQL;")
	db := newDB(t)
	r, _ := migrate.New(migrate.Config{DB: db, Dir: dir})
	if _, err := r.Apply(context.Background()); err == nil {
		t.Fatalf("expected error on bad SQL")
	}
	var rows []migrate.SchemaMigration
	_ = db.Find(&rows).Error
	// 001 should have committed; 002 should not.
	if len(rows) != 1 || rows[0].Version != 1 {
		t.Fatalf("expected only 001 applied, got %+v", rows)
	}
}

func TestRunner_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := migrate.New(migrate.Config{DB: nil, Dir: "."}); err == nil {
		t.Fatalf("expected nil DB error")
	}
}

func TestRunner_RejectsMissingDir(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	r, _ := migrate.New(migrate.Config{DB: db, Dir: t.TempDir() + "/nonexistent"})
	if _, err := r.Apply(context.Background()); err == nil {
		t.Fatalf("expected error for missing dir")
	}
}
