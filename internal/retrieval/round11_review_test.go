// round11_review_test.go — regression tests for two bugs Devin
// Review flagged on PR #20:
//
//  1. The batch handler's `runOne` recorded query analytics with
//     `topK = len(resp.Hits)` when the sub-request omitted `top_k`,
//     instead of the effective `DefaultTopK` that
//     `RetrieveWithSnapshotCached.resolveTopKAndPrivacy` actually used
//     to run the retrieve. Result: the `query_analytics.top_k`
//     column under-reported batch traffic whenever a slot returned
//     fewer hits than the configured default.
//  2. `CacheWarmer.Warm` constructed `RetrieveRequest` without
//     `Source`, and `RetrieveWithSnapshotCached` did not call the
//     analytics recorder at all — so the `cache_warm` source value
//     (Round-11 Task 9) was dead code and warm-up traffic was
//     invisible in `query_analytics`.
//
// Both fixes consolidate analytics recording inside
// `RetrieveWithSnapshotCached`, so it now fires for both the batch
// handler AND the cache warmer with the correct `Source` tag.
package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// TestBatch_AnalyticsTopKReportsDefaultWhenOmitted asserts that
// when a batch sub-request omits `top_k`, the analytics row records
// the effective `DefaultTopK` rather than `len(resp.Hits)`.
//
// Pre-fix: the recorder saw TopK=1 (== len(resp.Hits)) for a
// sub-request that omitted top_k against a corpus with one match.
// Post-fix: the recorder sees TopK=10 (== DefaultTopK).
func TestBatch_AnalyticsTopKReportsDefaultWhenOmitted(t *testing.T) {
	t.Parallel()

	vs := &slowVectorStore{
		hits: []storage.QdrantHit{
			{ID: "only-one", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	r := newRouter(t, h, "tenant-a")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		// Critical: TopK is intentionally omitted (0). The
		// pre-fix bug recorded `len(resp.Hits)` (== 1 with this
		// fake), not the resolved DefaultTopK (== 10).
		{Query: "batch-q1"},
	}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 analytics event, got %d", len(captured))
	}
	evt := captured[0]
	if evt.Source != retrieval.QueryAnalyticsSourceBatch {
		t.Fatalf("expected source=%q, got %q", retrieval.QueryAnalyticsSourceBatch, evt.Source)
	}
	// HitCount confirms the fake produced one result.
	if evt.HitCount != 1 {
		t.Fatalf("expected HitCount=1, got %d", evt.HitCount)
	}
	// TopK MUST be DefaultTopK (10), NOT len(resp.Hits) (1).
	// Pre-fix this assertion failed with `TopK=1`, which is the
	// regression Devin Review flagged on batch_handler.go:162-164.
	if evt.TopK != 10 {
		t.Fatalf("expected analytics TopK=10 (== DefaultTopK), got %d "+
			"(regression: batch handler was reporting len(resp.Hits) "+
			"instead of the effective DefaultTopK when the sub-request "+
			"omitted top_k)", evt.TopK)
	}
}

// TestBatch_AnalyticsTopKRespectsExplicitValue locks down the
// happy-path where an explicit sub-request top_k is honoured by
// the analytics recorder. Without this guard a future refactor
// might over-correct the previous bug and clobber explicit
// values.
func TestBatch_AnalyticsTopKRespectsExplicitValue(t *testing.T) {
	t.Parallel()

	vs := &slowVectorStore{
		hits: []storage.QdrantHit{
			{ID: "only-one", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	r := newRouter(t, h, "tenant-a")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "batch-q1", TopK: 25},
	}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 analytics event, got %d", len(captured))
	}
	if got := captured[0].TopK; got != 25 {
		t.Fatalf("expected analytics TopK=25 (explicit value), got %d", got)
	}
}

// TestCacheWarmer_AnalyticsTaggedCacheWarmSource asserts that
// CacheWarmer.Warm causes the analytics recorder to fire with
// `source = "cache_warm"`. Pre-fix the `QueryAnalyticsSourceCacheWarm`
// constant existed but was dead code: the warmer's constructed
// request had `Source == ""` (defaulted to "user" downstream) and
// `RetrieveWithSnapshotCached` did not call the recorder at all.
//
// Post-fix the warmer sets `Source = QueryAnalyticsSourceCacheWarm`
// and `RetrieveWithSnapshotCached` records analytics for every
// caller, so warm-up traffic is now visible in `query_analytics`
// and distinguishable from organic `user` and `batch` rows.
func TestCacheWarmer_AnalyticsTaggedCacheWarmSource(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a", "title": "v"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	resolver := stubResolver{snap: retrieval.PolicySnapshot{}}
	w, err := retrieval.NewCacheWarmer(h, resolver)
	if err != nil {
		t.Fatalf("NewCacheWarmer: %v", err)
	}

	summary := w.Warm(context.Background(), []retrieval.WarmTuple{
		{TenantID: "tenant-a", Query: "popular-query-1"},
		{TenantID: "tenant-b", Query: "popular-query-2"},
	})
	if summary.Total != 2 || summary.Succeeded != 2 || summary.Failed != 0 {
		t.Fatalf("expected 2 succeeded / 0 failed; got %+v", summary)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("expected 2 analytics events (one per warmed tuple), got %d "+
			"(regression: CacheWarmer.Warm was not reaching the analytics "+
			"recorder at all, so the cache_warm source value was dead code)",
			len(captured))
	}
	for i, evt := range captured {
		if evt.Source != retrieval.QueryAnalyticsSourceCacheWarm {
			t.Fatalf("event %d: expected source=%q, got %q (regression: "+
				"warmer was not tagging analytics so all warm-up traffic "+
				"defaulted to source=\"user\")",
				i, retrieval.QueryAnalyticsSourceCacheWarm, evt.Source)
		}
	}

	// Spot-check tenant attribution — without this, a future
	// refactor could collapse the per-tuple source onto the wrong
	// tenant_id.
	tenants := map[string]bool{}
	for _, evt := range captured {
		tenants[evt.TenantID] = true
	}
	if !tenants["tenant-a"] || !tenants["tenant-b"] {
		t.Fatalf("expected analytics events for both tenant-a and tenant-b, got %v", tenants)
	}
}
