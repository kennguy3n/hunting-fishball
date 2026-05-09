package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func newTantivyClientForTest(t *testing.T) *storage.TantivyClient {
	t.Helper()
	c, err := storage.NewTantivyClient(storage.TantivyConfig{MemOnly: true})
	if err != nil {
		t.Fatalf("NewTantivyClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestTantivy_NewTantivyClient_RequiresRootDirOrMemOnly(t *testing.T) {
	t.Parallel()

	_, err := storage.NewTantivyClient(storage.TantivyConfig{})
	if err == nil {
		t.Fatalf("expected error when neither RootDir nor MemOnly set")
	}
	c, err := storage.NewTantivyClient(storage.TantivyConfig{MemOnly: true})
	if err != nil {
		t.Fatalf("MemOnly: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestTantivy_TenantIsolation(t *testing.T) {
	t.Parallel()

	c := newTantivyClientForTest(t)
	ctx := context.Background()

	if err := c.Index(ctx, "", []storage.BM25Chunk{{ID: "x", Text: "y"}}); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("expected ErrMissingTenantScope on Index, got %v", err)
	}
	if _, err := c.Search(ctx, "", "y", 5); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("expected ErrMissingTenantScope on Search, got %v", err)
	}
	if err := c.Delete(ctx, "", "doc"); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("expected ErrMissingTenantScope on Delete, got %v", err)
	}
}

func TestTantivy_IndexAndSearch(t *testing.T) {
	t.Parallel()

	c := newTantivyClientForTest(t)
	ctx := context.Background()

	chunks := []storage.BM25Chunk{
		{ID: "a", DocumentID: "doc-1", Title: "Quarterly Report", Text: "revenue grew 12 percent in Q3"},
		{ID: "b", DocumentID: "doc-1", Title: "Quarterly Report", Text: "operating expenses were flat"},
		{ID: "c", DocumentID: "doc-2", Title: "Engineering Notes", Text: "kafka rebalancing latency was tuned"},
	}
	if err := c.Index(ctx, "tenant-a", chunks); err != nil {
		t.Fatalf("Index: %v", err)
	}

	hits, err := c.Search(ctx, "tenant-a", "revenue", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("expected first hit to be chunk a, got %+v", hits)
	}
	if hits[0].DocumentID != "doc-1" {
		t.Fatalf("document_id: %q", hits[0].DocumentID)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("score should be positive: %v", hits[0].Score)
	}

	hits, err = c.Search(ctx, "tenant-a", "kafka", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "c" {
		t.Fatalf("expected first hit to be chunk c, got %+v", hits)
	}
}

func TestTantivy_TenantsAreSeparated(t *testing.T) {
	t.Parallel()

	c := newTantivyClientForTest(t)
	ctx := context.Background()

	if err := c.Index(ctx, "tenant-a", []storage.BM25Chunk{{ID: "a", DocumentID: "d", Text: "alpha bravo"}}); err != nil {
		t.Fatalf("Index a: %v", err)
	}
	if err := c.Index(ctx, "tenant-b", []storage.BM25Chunk{{ID: "b", DocumentID: "d", Text: "alpha bravo"}}); err != nil {
		t.Fatalf("Index b: %v", err)
	}

	hitsA, err := c.Search(ctx, "tenant-a", "alpha", 10)
	if err != nil {
		t.Fatalf("search A: %v", err)
	}
	if len(hitsA) != 1 || hitsA[0].ID != "a" {
		t.Fatalf("tenant-a leaked: %+v", hitsA)
	}
	hitsB, err := c.Search(ctx, "tenant-b", "alpha", 10)
	if err != nil {
		t.Fatalf("search B: %v", err)
	}
	if len(hitsB) != 1 || hitsB[0].ID != "b" {
		t.Fatalf("tenant-b leaked: %+v", hitsB)
	}
}

func TestTantivy_Delete(t *testing.T) {
	t.Parallel()

	c := newTantivyClientForTest(t)
	ctx := context.Background()

	if err := c.Index(ctx, "tenant-a", []storage.BM25Chunk{
		{ID: "a", DocumentID: "doc-1", Text: "alpha"},
		{ID: "b", DocumentID: "doc-1", Text: "beta"},
		{ID: "c", DocumentID: "doc-2", Text: "alpha"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	if err := c.Delete(ctx, "tenant-a", "doc-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	hits, err := c.Search(ctx, "tenant-a", "alpha", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "c" {
		t.Fatalf("expected only chunk c, got %+v", hits)
	}

	// Delete is idempotent: second call succeeds.
	if err := c.Delete(ctx, "tenant-a", "doc-1"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if err := c.Delete(ctx, "tenant-a", "missing"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestTantivy_PersistsAcrossOpen(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "bm25")
	c, err := storage.NewTantivyClient(storage.TantivyConfig{RootDir: dir})
	if err != nil {
		t.Fatalf("NewTantivyClient: %v", err)
	}
	if err := c.Index(context.Background(), "tenant-a", []storage.BM25Chunk{
		{ID: "a", DocumentID: "doc", Text: "persistent storage"},
	}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c2, err := storage.NewTantivyClient(storage.TantivyConfig{RootDir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	hits, err := c2.Search(context.Background(), "tenant-a", "persistent", 5)
	if err != nil {
		t.Fatalf("Search after reopen: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "a" {
		t.Fatalf("expected persistent chunk a, got %+v", hits)
	}
}

func TestTantivy_EmptyQueryReturnsNothing(t *testing.T) {
	t.Parallel()

	c := newTantivyClientForTest(t)
	ctx := context.Background()
	if err := c.Index(ctx, "tenant-a", []storage.BM25Chunk{{ID: "a", DocumentID: "d", Text: "hello"}}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	hits, err := c.Search(ctx, "tenant-a", "", 5)
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected zero hits for empty query, got %+v", hits)
	}
}

func TestTantivy_NoFieldInjection(t *testing.T) {
	t.Parallel()

	// MatchQuery applies the field's analyzer to the input string;
	// query-string operators ("title:foo", "AND", "OR") are treated as
	// ordinary tokens, so a user-supplied query cannot inject a
	// field-scoped query that escapes the tenant boundary. This is the
	// reason we picked MatchQuery over QueryStringQuery.
	c := newTantivyClientForTest(t)
	ctx := context.Background()
	if err := c.Index(ctx, "tenant-a", []storage.BM25Chunk{{ID: "a", DocumentID: "d", Text: "operator notes"}}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if _, err := c.Search(ctx, "tenant-a", "title:doesnotexist", 5); err != nil {
		t.Fatalf("colon-bearing query must not error: %v", err)
	}
	if _, err := c.Search(ctx, "tenant-a", "AND OR NOT", 5); err != nil {
		t.Fatalf("operator-only query must not error: %v", err)
	}
}
