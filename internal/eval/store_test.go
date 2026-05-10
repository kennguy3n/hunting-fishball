package eval

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// sqliteEvalSchema mirrors migrations/012_eval_suites.sql in
// SQLite-compatible types so unit tests can run without Postgres.
const sqliteEvalSchema = `
CREATE TABLE eval_suites (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    name         TEXT NOT NULL,
    corpus       TEXT NOT NULL DEFAULT '[]',
    default_k    INTEGER NOT NULL DEFAULT 0,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_eval_suites_tenant_name ON eval_suites (tenant_id, name);
`

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.Exec(sqliteEvalSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestSuiteRepository_SaveAndGet(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	repo := NewSuiteRepository(db)
	suite := EvalSuite{
		Name:     "demo",
		TenantID: "tenant-a",
		DefaultK: 5,
		Cases: []EvalCase{
			{Query: "q1", ExpectedChunkIDs: []string{"a", "b"}},
		},
	}
	ctx := context.Background()
	if err := repo.Save(ctx, suite); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.Get(ctx, "tenant-a", "demo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DefaultK != 5 || len(got.Cases) != 1 || got.Cases[0].Query != "q1" {
		t.Fatalf("roundtrip: %+v", got)
	}
}

func TestSuiteRepository_UpsertOverwrites(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	repo := NewSuiteRepository(db)
	ctx := context.Background()
	original := EvalSuite{
		Name:     "demo",
		TenantID: "tenant-a",
		Cases:    []EvalCase{{Query: "v1"}},
	}
	if err := repo.Save(ctx, original); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	updated := EvalSuite{
		Name:     "demo",
		TenantID: "tenant-a",
		Cases:    []EvalCase{{Query: "v2"}},
	}
	if err := repo.Save(ctx, updated); err != nil {
		t.Fatalf("upsert Save: %v", err)
	}

	got, err := repo.Get(ctx, "tenant-a", "demo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Cases) != 1 || got.Cases[0].Query != "v2" {
		t.Fatalf("upsert overwrite: %+v", got)
	}
}

func TestSuiteRepository_Get_NotFound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	repo := NewSuiteRepository(db)
	_, err := repo.Get(context.Background(), "tenant-a", "missing")
	if err == nil {
		t.Fatal("expected ErrSuiteNotFound")
	}
	if err != ErrSuiteNotFound {
		t.Fatalf("err: %v", err)
	}
}

func TestSuiteRepository_TenantIsolation(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	repo := NewSuiteRepository(db)
	ctx := context.Background()
	_ = repo.Save(ctx, EvalSuite{Name: "demo", TenantID: "tenant-a", Cases: []EvalCase{{Query: "q"}}})
	_ = repo.Save(ctx, EvalSuite{Name: "demo", TenantID: "tenant-b", Cases: []EvalCase{{Query: "q"}}})

	a, err := repo.List(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("List a: %v", err)
	}
	b, err := repo.List(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("List b: %v", err)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("isolation: a=%d b=%d", len(a), len(b))
	}

	if _, err := repo.Get(ctx, "tenant-a", "demo"); err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if _, err := repo.Get(ctx, "tenant-c", "demo"); err != ErrSuiteNotFound {
		t.Fatalf("cross-tenant leak: %v", err)
	}
}
