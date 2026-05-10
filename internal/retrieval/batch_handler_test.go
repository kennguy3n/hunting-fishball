package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

type slowVectorStore struct {
	mu         sync.Mutex
	delay      time.Duration
	hits       []storage.QdrantHit
	concurrent int32
	maxObs     int32
}

func (f *slowVectorStore) Search(ctx context.Context, _ string, _ []float32, _ storage.SearchOpts) ([]storage.QdrantHit, error) {
	now := atomic.AddInt32(&f.concurrent, 1)
	defer atomic.AddInt32(&f.concurrent, -1)
	for {
		max := atomic.LoadInt32(&f.maxObs)
		if now <= max || atomic.CompareAndSwapInt32(&f.maxObs, max, now) {
			break
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, nil
}

func TestBatch_HappyPath_AllSucceed(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{hits: []storage.QdrantHit{{ID: "a:b:c", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1, 2}}})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "q1"}, {Query: "q2"}, {Query: "q3"},
	}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got retrieval.BatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got.Results))
	}
	for _, r := range got.Results {
		if r.Error != "" || r.Response == nil {
			t.Fatalf("result %d: err=%q resp=%v", r.Index, r.Error, r.Response)
		}
	}
}

func TestBatch_RequiresTenant(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1}}})
	r := newRouter(t, h, "")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{{Query: "q"}}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestBatch_BadJSON(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1}}})
	r := newRouter(t, h, "tenant-a")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader([]byte("{not-json")))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestBatch_EmptyRejected(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1}}})
	r := newRouter(t, h, "tenant-a")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestBatch_TooLargeRejected(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1}}})
	r := newRouter(t, h, "tenant-a")
	reqs := make([]retrieval.RetrieveRequest, retrieval.MaxBatchSize+1)
	for i := range reqs {
		reqs[i] = retrieval.RetrieveRequest{Query: "q"}
	}
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: reqs})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestBatch_ConcurrencyCapped(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{delay: 30 * time.Millisecond, hits: []storage.QdrantHit{{ID: "a", Payload: map[string]any{"tenant_id": "tenant-a"}}}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1}}})
	reqs := make([]retrieval.RetrieveRequest, 8)
	for i := range reqs {
		reqs[i] = retrieval.RetrieveRequest{Query: "q"}
	}
	resp := h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{Requests: reqs, MaxParallel: 2})
	if len(resp.Results) != 8 {
		t.Fatalf("expected 8 results, got %d", len(resp.Results))
	}
	if got := atomic.LoadInt32(&vs.maxObs); got > 2 {
		t.Fatalf("expected concurrency<=2, observed %d", got)
	}
}

// TestBatch_CacheHitsServeWithoutFanOut is the regression test for the
// Phase 8 / Task 12 cache-bypass bug surfaced by Devin Review on
// PR #12. Pre-fix, batch retrieve called RetrieveWithSnapshot, which
// is the simulator entry point and intentionally skips the semantic
// cache; a hot dashboard query that fans out across N panels paid the
// full Qdrant + BM25 + rerank cost on every slot.
//
// Post-fix, batch sub-requests run through RetrieveWithSnapshotCached,
// so an entry written by an earlier /v1/retrieve (or by the first slot
// in the batch) is reused by every subsequent slot. We assert this two
// ways: (1) every slot resolves to the cached payload, and (2) the
// vector store is never touched for the cached query.
func TestBatch_CacheHitsServeWithoutFanOut(t *testing.T) {
	t.Parallel()
	cached := &storage.CachedResult{
		Hits: []storage.CachedHit{{ID: "cached-1", Score: 0.99, TenantID: "tenant-a", Title: "from-cache"}},
	}
	cache := &fakeCache{getValue: cached}
	vs := &slowVectorStore{
		hits: []storage.QdrantHit{{ID: "fresh-1", Score: 0.5, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
		Cache:       cache,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	resp := h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "popular-dashboard-query"},
		{Query: "popular-dashboard-query"},
		{Query: "popular-dashboard-query"},
	}})

	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(resp.Results))
	}
	for i, r := range resp.Results {
		if r.Error != "" || r.Response == nil {
			t.Fatalf("slot %d: err=%q resp=%v", i, r.Error, r.Response)
		}
		if len(r.Response.Hits) != 1 || r.Response.Hits[0].ID != "cached-1" {
			t.Fatalf("slot %d: expected cached hit, got %+v", i, r.Response.Hits)
		}
	}
	// Cache.Get must run for every slot. Cache.Set must never run
	// because every slot took the hit path.
	if g := atomic.LoadInt32(&cache.getCalls); g != 3 {
		t.Fatalf("expected 3 cache GETs (one per slot), got %d", g)
	}
	if s := atomic.LoadInt32(&cache.setCalls); s != 0 {
		t.Fatalf("expected 0 cache SETs on cache-hit path, got %d", s)
	}
	// The vector store must never be touched for a fully-cached batch.
	// Pre-fix this counter was 3 (one fan-out per slot).
	if got := atomic.LoadInt32(&vs.concurrent); got != 0 {
		t.Fatalf("vector store should not run on fully-cached batch; got concurrent=%d", got)
	}
}

// TestBatch_CacheMissPopulatesCache verifies the cache-write half of
// the cache-aware path: a cold slot fans out, then writes the result
// so subsequent slots can hit. Without this assertion the bug could
// recur (e.g. if a future refactor restored RetrieveWithSnapshot in
// runOne) without the read-side test catching it, since the read-side
// test uses a pre-populated fakeCache.
func TestBatch_CacheMissPopulatesCache(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{}
	vs := &slowVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		Cache:       cache,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	resp := h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "first-call"},
	}})
	if len(resp.Results) != 1 || resp.Results[0].Response == nil {
		t.Fatalf("expected 1 successful result, got %+v", resp)
	}
	if g := atomic.LoadInt32(&cache.getCalls); g != 1 {
		t.Fatalf("expected 1 cache GET, got %d", g)
	}
	if s := atomic.LoadInt32(&cache.setCalls); s != 1 {
		t.Fatalf("expected 1 cache SET on cache-miss path, got %d", s)
	}
}

func TestBatch_PerRequestQueryRequired(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1}}})
	resp := h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "q1"}, {Query: ""},
	}})
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].Error != "" {
		t.Fatalf("first should succeed: %v", resp.Results[0].Error)
	}
	if resp.Results[1].Error == "" {
		t.Fatalf("second should report error")
	}
}
