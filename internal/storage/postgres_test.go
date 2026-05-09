package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func newPGStore(t *testing.T) *storage.PostgresStore {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	s, err := storage.NewPostgresStore(db)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	if err := s.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}

	return s
}

func TestPostgresStore_UpsertAndFetch(t *testing.T) {
	t.Parallel()

	s := newPGStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	chunks := []storage.Chunk{
		{
			ID: "tenant-a:doc-1:b1", TenantID: "tenant-a", SourceID: "src-1",
			DocumentID: "doc-1", BlockID: "b1", ContentHash: "h1",
			Title: "Hello", Text: "hello world", Connector: "google_drive", PrivacyLabel: "remote",
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "tenant-a:doc-1:b2", TenantID: "tenant-a", SourceID: "src-1",
			DocumentID: "doc-1", BlockID: "b2", ContentHash: "h1",
			Text: "more text", CreatedAt: now, UpdatedAt: now,
		},
	}

	if err := s.UpsertChunks(ctx, "tenant-a", chunks); err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}
	out, err := s.FetchChunks(ctx, "tenant-a", []string{"tenant-a:doc-1:b1", "tenant-a:doc-1:b2"})
	if err != nil {
		t.Fatalf("FetchChunks: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len: %d", len(out))
	}
}

func TestPostgresStore_TenantIsolation(t *testing.T) {
	t.Parallel()

	s := newPGStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.UpsertChunks(ctx, "tenant-a", []storage.Chunk{
		{ID: "x:doc:b", TenantID: "tenant-a", SourceID: "s", DocumentID: "doc", BlockID: "b", ContentHash: "h", Text: "x", CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}

	// Cross-tenant read returns no rows.
	out, err := s.FetchChunks(ctx, "tenant-b", []string{"x:doc:b"})
	if err != nil {
		t.Fatalf("FetchChunks: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("cross-tenant leak: %+v", out)
	}
}

func TestPostgresStore_RejectsEmptyTenant(t *testing.T) {
	t.Parallel()

	s := newPGStore(t)
	ctx := context.Background()

	if err := s.UpsertChunks(ctx, "", []storage.Chunk{{ID: "x"}}); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("UpsertChunks: %v", err)
	}
	if _, err := s.FetchChunks(ctx, "", []string{"x"}); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("FetchChunks: %v", err)
	}
	if _, err := s.LatestHashForDocument(ctx, "", "doc"); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("LatestHashForDocument: %v", err)
	}
	if _, err := s.DeleteByDocument(ctx, "", "doc"); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("DeleteByDocument: %v", err)
	}
}

func TestPostgresStore_LatestHashForDocument(t *testing.T) {
	t.Parallel()

	s := newPGStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.UpsertChunks(ctx, "tenant-a", []storage.Chunk{
		{ID: "tenant-a:doc:b1", TenantID: "tenant-a", SourceID: "s", DocumentID: "doc", BlockID: "b1", ContentHash: "h1", Text: "x", CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}
	got, err := s.LatestHashForDocument(ctx, "tenant-a", "doc")
	if err != nil {
		t.Fatalf("LatestHashForDocument: %v", err)
	}
	if got != "h1" {
		t.Fatalf("hash: %q", got)
	}

	got, err = s.LatestHashForDocument(ctx, "tenant-a", "missing")
	if err != nil {
		t.Fatalf("LatestHashForDocument: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestPostgresStore_DeleteByDocument(t *testing.T) {
	t.Parallel()

	s := newPGStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.UpsertChunks(ctx, "tenant-a", []storage.Chunk{
		{ID: "tenant-a:doc:b1", TenantID: "tenant-a", SourceID: "s", DocumentID: "doc", BlockID: "b1", ContentHash: "h", Text: "x", CreatedAt: now, UpdatedAt: now},
		{ID: "tenant-a:doc:b2", TenantID: "tenant-a", SourceID: "s", DocumentID: "doc", BlockID: "b2", ContentHash: "h", Text: "y", CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}
	ids, err := s.DeleteByDocument(ctx, "tenant-a", "doc")
	if err != nil {
		t.Fatalf("DeleteByDocument: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids: %d", len(ids))
	}
}

func TestPostgresStore_NewPostgresStore_NilDB(t *testing.T) {
	t.Parallel()

	if _, err := storage.NewPostgresStore(nil); err == nil {
		t.Fatal("expected error")
	}
}
