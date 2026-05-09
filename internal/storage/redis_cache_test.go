package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func newCacheWithMiniredis(t *testing.T) (*storage.SemanticCache, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	cache, err := storage.NewSemanticCache(storage.SemanticCacheConfig{
		Client:     rc,
		KeyPrefix:  "hf:",
		DefaultTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSemanticCache: %v", err)
	}

	return cache, mr, rc
}

func TestSemanticCache_TenantScopeGuards(t *testing.T) {
	t.Parallel()

	cache, _, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	if _, err := cache.Get(ctx, "", "ch", []float32{0.1}, ""); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("Get: %v", err)
	}
	if err := cache.Set(ctx, "", "ch", []float32{0.1}, "", &storage.CachedResult{}, 0); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("Set: %v", err)
	}
	if err := cache.Invalidate(ctx, "", []string{"chunk"}); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("Invalidate: %v", err)
	}
}

func TestSemanticCache_NewCache_RequiresClient(t *testing.T) {
	t.Parallel()

	if _, err := storage.NewSemanticCache(storage.SemanticCacheConfig{}); err == nil {
		t.Fatalf("expected error when Client nil")
	}
}

func TestSemanticCache_KeyPerTenantIsolation(t *testing.T) {
	t.Parallel()

	cache, _, _ := newCacheWithMiniredis(t)
	emb := []float32{0.1, 0.2, 0.3}
	keyA := cache.CacheKey("tenant-a", "ch", emb, "scope-x")
	keyB := cache.CacheKey("tenant-b", "ch", emb, "scope-x")
	if keyA == keyB {
		t.Fatalf("tenant-a and tenant-b shared a cache key")
	}

	// Same tenant + different channel → different key.
	if cache.CacheKey("tenant-a", "ch", emb, "scope-x") == cache.CacheKey("tenant-a", "ch2", emb, "scope-x") {
		t.Fatalf("channel did not change cache key")
	}
	// Same tenant + different scope → different key.
	if cache.CacheKey("tenant-a", "ch", emb, "scope-x") == cache.CacheKey("tenant-a", "ch", emb, "scope-y") {
		t.Fatalf("scope did not change cache key")
	}
}

func TestSemanticCache_RoundTrip(t *testing.T) {
	t.Parallel()

	cache, _, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	res := &storage.CachedResult{
		Hits: []storage.CachedHit{{ID: "chunk-1", Score: 0.9, TenantID: "tenant-a", Title: "hello"}},
	}
	if err := cache.Set(ctx, "tenant-a", "ch", []float32{0.1, 0.2}, "scope", res, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := cache.Get(ctx, "tenant-a", "ch", []float32{0.1, 0.2}, "scope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || len(got.Hits) != 1 || got.Hits[0].ID != "chunk-1" {
		t.Fatalf("got: %+v", got)
	}
	if got.CachedAt.IsZero() {
		t.Fatalf("CachedAt should be set")
	}
	if len(got.ChunkIDs) != 1 || got.ChunkIDs[0] != "chunk-1" {
		t.Fatalf("ChunkIDs: %v", got.ChunkIDs)
	}
}

func TestSemanticCache_GetMiss(t *testing.T) {
	t.Parallel()

	cache, _, _ := newCacheWithMiniredis(t)
	got, err := cache.Get(context.Background(), "tenant-a", "ch", []float32{0.1}, "scope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected miss, got %+v", got)
	}
}

func TestSemanticCache_TTL(t *testing.T) {
	t.Parallel()

	cache, mr, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	res := &storage.CachedResult{
		Hits: []storage.CachedHit{{ID: "chunk-1", Score: 0.9}},
	}
	if err := cache.Set(ctx, "tenant-a", "ch", []float32{0.1}, "scope", res, 1*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, err := cache.Get(ctx, "tenant-a", "ch", []float32{0.1}, "scope"); err != nil || v == nil {
		t.Fatalf("expected hit before expiry: %+v %v", v, err)
	}
	mr.FastForward(2 * time.Second)
	got, err := cache.Get(ctx, "tenant-a", "ch", []float32{0.1}, "scope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected expired entry, got %+v", got)
	}
}

func TestSemanticCache_Invalidate(t *testing.T) {
	t.Parallel()

	cache, _, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	res := &storage.CachedResult{
		Hits: []storage.CachedHit{
			{ID: "chunk-1", Score: 0.9},
			{ID: "chunk-2", Score: 0.7},
		},
	}
	if err := cache.Set(ctx, "tenant-a", "ch", []float32{0.1}, "scope-x", res, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// A second cache entry referencing one shared chunk + one
	// distinct chunk.
	res2 := &storage.CachedResult{
		Hits: []storage.CachedHit{
			{ID: "chunk-2", Score: 0.6},
			{ID: "chunk-3", Score: 0.5},
		},
	}
	if err := cache.Set(ctx, "tenant-a", "ch", []float32{0.2}, "scope-y", res2, 0); err != nil {
		t.Fatalf("Set 2: %v", err)
	}

	// Invalidate chunk-2: both cache entries should be dropped.
	if err := cache.Invalidate(ctx, "tenant-a", []string{"chunk-2"}); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if v, _ := cache.Get(ctx, "tenant-a", "ch", []float32{0.1}, "scope-x"); v != nil {
		t.Fatalf("entry x should have been invalidated")
	}
	if v, _ := cache.Get(ctx, "tenant-a", "ch", []float32{0.2}, "scope-y"); v != nil {
		t.Fatalf("entry y should have been invalidated")
	}
}

func TestSemanticCache_Invalidate_NoMatchingChunks(t *testing.T) {
	t.Parallel()

	cache, _, _ := newCacheWithMiniredis(t)
	if err := cache.Invalidate(context.Background(), "tenant-a", []string{"unknown"}); err != nil {
		t.Fatalf("Invalidate (no match): %v", err)
	}
}

func TestSemanticCache_CorruptEntryIsTreatedAsMiss(t *testing.T) {
	t.Parallel()

	cache, _, rc := newCacheWithMiniredis(t)
	ctx := context.Background()

	key := cache.CacheKey("tenant-a", "ch", []float32{0.1}, "scope")
	if err := rc.Set(ctx, key, "this-is-not-json", time.Hour).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := cache.Get(ctx, "tenant-a", "ch", []float32{0.1}, "scope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected miss, got %+v", got)
	}
}
