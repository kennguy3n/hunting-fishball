package admin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// sqliteSourcesSchema is a SQLite-compatible mirror of
// migrations/002_sources.sql. The production schema uses Postgres
// types (jsonb, char(26), tsz); SQLite stores them as TEXT/DATETIME,
// which is good enough for repository-level behaviour.
const sqliteSourcesSchema = `
CREATE TABLE sources (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    connector_type  TEXT NOT NULL,
    config          TEXT NOT NULL DEFAULT '{}',
    scopes          TEXT NOT NULL DEFAULT '[]',
    status          TEXT NOT NULL DEFAULT 'active',
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
);
CREATE INDEX idx_sources_tenant_id ON sources (tenant_id, id DESC);
CREATE INDEX idx_sources_tenant_status ON sources (tenant_id, status);
`

// newSQLiteSourceRepo spins up an in-memory SQLite-backed repo with
// the sources schema applied. Mirrors the audit_test pattern.
func newSQLiteSourceRepo(t *testing.T) (*admin.SourceRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteSourcesSchema).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return admin.NewSourceRepository(db), db
}

func TestSourceRepository_CreateAndGet(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", admin.JSONMap{"site": "x"}, []string{"drive-1"})
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, "tenant-a", src.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ConnectorType != "google-drive" {
		t.Fatalf("ConnectorType: %q", got.ConnectorType)
	}
	if got.Config["site"] != "x" {
		t.Fatalf("Config not round-tripped: %+v", got.Config)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != "drive-1" {
		t.Fatalf("Scopes: %+v", got.Scopes)
	}
}

func TestSourceRepository_TenantIsolationGet(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.Get(ctx, "tenant-b", src.ID); !errors.Is(err, admin.ErrSourceNotFound) {
		t.Fatalf("cross-tenant Get must return ErrSourceNotFound, got %v", err)
	}
}

func TestSourceRepository_List_TenantOnly(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	for _, tenant := range []string{"tenant-a", "tenant-b", "tenant-a"} {
		s := admin.NewSource(tenant, "slack", nil, nil)
		if err := repo.Create(ctx, s); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	got, err := repo.List(ctx, admin.ListFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows for tenant-a, got %d", len(got))
	}
	for _, s := range got {
		if s.TenantID != "tenant-a" {
			t.Fatalf("tenant leak: %+v", s)
		}
	}
}

func TestSourceRepository_Update_PausedAndScopes(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", nil, []string{"d1"})
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	paused := admin.SourceStatusPaused
	scopes := []string{"d1", "d2"}
	got, err := repo.Update(ctx, "tenant-a", src.ID, admin.UpdatePatch{Status: &paused, Scopes: &scopes})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Status != admin.SourceStatusPaused {
		t.Fatalf("Status: %q", got.Status)
	}
	if len(got.Scopes) != 2 {
		t.Fatalf("Scopes: %+v", got.Scopes)
	}
}

func TestSourceRepository_Update_RejectsRemovingViaPatch(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rem := admin.SourceStatusRemoving
	if _, err := repo.Update(ctx, "tenant-a", src.ID, admin.UpdatePatch{Status: &rem}); err == nil {
		t.Fatal("expected error: removing must go through MarkRemoving")
	}
}

func TestSourceRepository_MarkRemoving_Idempotent(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.MarkRemoving(ctx, "tenant-a", src.ID); err != nil {
		t.Fatalf("first MarkRemoving: %v", err)
	}
	got, err := repo.MarkRemoving(ctx, "tenant-a", src.ID)
	if err != nil {
		t.Fatalf("second MarkRemoving: %v", err)
	}
	if got.Status != admin.SourceStatusRemoving {
		t.Fatalf("Status: %q", got.Status)
	}
}

func TestSourceRepository_MarkRemoved(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.MarkRemoving(ctx, "tenant-a", src.ID); err != nil {
		t.Fatalf("MarkRemoving: %v", err)
	}
	if err := repo.MarkRemoved(ctx, "tenant-a", src.ID); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	got, _ := repo.Get(ctx, "tenant-a", src.ID)
	if got.Status != admin.SourceStatusRemoved {
		t.Fatalf("Status: %q", got.Status)
	}
}

func TestSourceRepository_Update_NotFound(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()
	paused := admin.SourceStatusPaused
	if _, err := repo.Update(ctx, "tenant-a", "missing", admin.UpdatePatch{Status: &paused}); !errors.Is(err, admin.ErrSourceNotFound) {
		t.Fatalf("Update missing: %v", err)
	}
}
