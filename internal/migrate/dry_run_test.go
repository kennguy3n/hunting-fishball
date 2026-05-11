// Round-12 Task 9 — migration dry-run CI gate.
//
// TestMigrate_DryRun runs migrate.Runner.DryRun() against the live
// migrations/ directory inside a fresh SQLite database. The
// assertion is intentionally narrow: every committed migration is
// readable, has a parseable numeric prefix, and shows up in the
// pending list. We don't try to Apply() the migrations against
// SQLite because the real schema is Postgres-specific (JSONB,
// TIMESTAMPTZ, partial indexes with WHERE) — that's the production
// runner's job. Catching a malformed filename (e.g. "abc_foo.sql"
// or a duplicate numeric prefix) at PR time is what this gate is
// for.
package migrate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/migrate"
)

func TestMigrate_DryRun(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	migDir := filepath.Join(repoRoot, "migrations")

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}

	r, err := migrate.New(migrate.Config{DB: db, Dir: migDir})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	pending, err := r.DryRun(context.Background())
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	want := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		want++
	}
	if len(pending) != want {
		t.Fatalf("dry-run pending=%d want=%d (every committed migration must be discoverable)",
			len(pending), want)
	}

	// Round-12 Task 9: every pending migration must have a parseable
	// version prefix and a non-empty SQL body. A trailing-newline or
	// empty .sql file is almost certainly a mid-rebase artifact and
	// should fail the gate.
	for _, m := range pending {
		if m.Version <= 0 {
			t.Errorf("migration %q has non-positive version %d", m.Name, m.Version)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %03d_%s has empty SQL body", m.Version, m.Name)
		}
	}
}
