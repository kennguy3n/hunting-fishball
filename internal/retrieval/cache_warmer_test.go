// cache_warmer_test.go — regression coverage for the
// CacheWarmer cache-bypass bug Devin Review surfaced on PR #16.
//
// Pre-fix, CacheWarmer.Warm called Handler.RetrieveWithSnapshot,
// which the handler's own godoc documents as the simulator path
// that "intentionally NOT consulted on this path" the semantic
// cache (handler.go:606-628). Result: every warm-cache call paid
// the full Qdrant + BM25 + rerank cost and wrote zero entries —
// i.e. the entire Task 9 feature was a silent no-op.
//
// Post-fix, the warmer routes through RetrieveWithSnapshotCached
// so the handler's post-retrieve cache.Set populates Redis. This
// test asserts that contract by counting cache.Set calls on a
// fakeCache.
package retrieval_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// TestCacheWarmer_PopulatesCache is the read-the-bug-back-in-a-
// commit regression test. A successful Warm() call must invoke
// cache.Set at least once. Pre-fix this counter stayed at zero.
func TestCacheWarmer_PopulatesCache(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{}
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a", "title": "v"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
		Cache:       cache,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	resolver := stubResolver{snap: retrieval.PolicySnapshot{}}

	w, err := retrieval.NewCacheWarmer(h, resolver)
	if err != nil {
		t.Fatalf("NewCacheWarmer: %v", err)
	}

	summary := w.Warm(context.Background(), []retrieval.WarmTuple{
		{TenantID: "tenant-a", Query: "popular-dashboard-query"},
	})

	if summary.Total != 1 || summary.Succeeded != 1 || summary.Failed != 0 {
		t.Fatalf("expected 1 succeeded / 0 failed; got %+v", summary)
	}
	if got := atomic.LoadInt32(&cache.setCalls); got < 1 {
		t.Fatalf("expected at least 1 cache.Set call after warming; got %d "+
			"(this is the regression: warmer routing through "+
			"RetrieveWithSnapshot bypassed the handler's cache.Set "+
			"bookend)", got)
	}
	// Get must also have been called (cache-miss path). Without
	// a Get the warmer wasn't going through the cache-aware
	// entrypoint at all.
	if got := atomic.LoadInt32(&cache.getCalls); got < 1 {
		t.Fatalf("expected at least 1 cache.Get call (cache-aware path); got %d", got)
	}
}

// TestCacheWarmer_WarmEnablesCacheHits closes the loop: after the
// warmer populates the cache, a follow-up retrieval against the
// same query / tenant must serve from cache without re-running
// the vector-store fan-out. Without the bug fix the warmer would
// have written nothing and this follow-up call would still pay
// the full fan-out cost.
func TestCacheWarmer_WarmEnablesCacheHits(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{}
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a", "title": "v"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
		Cache:       cache,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	resolver := stubResolver{snap: retrieval.PolicySnapshot{}}

	w, err := retrieval.NewCacheWarmer(h, resolver)
	if err != nil {
		t.Fatalf("NewCacheWarmer: %v", err)
	}

	// First call — populates the cache (1 Get miss, 1 Set).
	_ = w.Warm(context.Background(), []retrieval.WarmTuple{
		{TenantID: "tenant-a", Query: "hot"},
	})
	setsAfterWarm := atomic.LoadInt32(&cache.setCalls)
	if setsAfterWarm < 1 {
		t.Fatalf("warmer did not Set anything: %d", setsAfterWarm)
	}
	// Simulate a populated cache by stamping fakeCache's getValue
	// from the lastSet payload, so the next Get returns it as a
	// hit. This mirrors what Redis would do in production — the
	// in-test fakeCache doesn't auto-mirror Set→Get because each
	// test in this package uses different scope hashes.
	cache.mu.Lock()
	cache.getValue = cache.lastSet
	cache.mu.Unlock()

	// Second call — should hit cache, NOT fan out.
	prevVSCalls := vs.calls
	_ = w.Warm(context.Background(), []retrieval.WarmTuple{
		{TenantID: "tenant-a", Query: "hot"},
	})
	if vs.calls != prevVSCalls {
		t.Fatalf("second warm of the same query should serve from "+
			"cache without touching the vector store; vs.calls "+
			"went %d → %d", prevVSCalls, vs.calls)
	}
}
