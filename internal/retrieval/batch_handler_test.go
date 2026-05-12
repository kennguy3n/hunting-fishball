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

// TestBatch_ExplainPropagatesToHits — Round-9 Task 7.
// The single-request /v1/retrieve endpoint honours the per-request
// `explain: true` flag and surfaces a per-hit `explain` block for
// admin callers. The batch endpoint must do the same — pre-fix the
// flag was silently dropped because runPipelineFromVec didn't emit
// the block.
func TestBatch_ExplainPropagatesToHits(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{hits: []storage.QdrantHit{
		{ID: "doc:b1", Score: 0.91, Payload: map[string]any{
			"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
			"title": "T", "text": "x", "privacy_label": "internal",
		}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:       vs,
		Embedder:          &fakeEmbedder{vec: []float32{1, 2, 3}},
		ExplainEnvEnabled: true, // skip the role check
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	resp := h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{
		Requests: []retrieval.RetrieveRequest{{Query: "q1", Explain: true}},
	})
	if len(resp.Results) != 1 || resp.Results[0].Response == nil {
		t.Fatalf("expected 1 successful result; got %+v", resp)
	}
	hits := resp.Results[0].Response.Hits
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit; got 0")
	}
	if hits[0].Explain == nil {
		t.Fatalf("expected hit.Explain to be populated for explain=true; got nil")
	}
}

// TestBatch_ExplainOmittedWhenFlagFalse confirms the explain block
// stays off when the caller did not opt in.
func TestBatch_ExplainOmittedWhenFlagFalse(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{hits: []storage.QdrantHit{
		{ID: "doc:b1", Score: 0.5, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:       vs,
		Embedder:          &fakeEmbedder{vec: []float32{1}},
		ExplainEnvEnabled: true,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	resp := h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{
		Requests: []retrieval.RetrieveRequest{{Query: "q1"}}, // no Explain flag
	})
	hits := resp.Results[0].Response.Hits
	for _, h := range hits {
		if h.Explain != nil {
			t.Fatalf("expected no explain block when flag is false; got %+v", h.Explain)
		}
	}
}

// slowCache.Set blocks for `delay` before returning. Used to assert
// the warm-on-miss path doesn't pad the response latency.
type slowCache struct {
	delay       time.Duration
	setStarted  chan struct{} // closed when first Set is entered
	setFinished chan struct{} // closed when first Set returns
	once        sync.Once
	onceFin     sync.Once
}

func newSlowCache(delay time.Duration) *slowCache {
	return &slowCache{
		delay:       delay,
		setStarted:  make(chan struct{}),
		setFinished: make(chan struct{}),
	}
}

func (s *slowCache) Get(_ context.Context, _ string, _ string, _ []float32, _ string) (*storage.CachedResult, error) {
	return nil, nil
}

func (s *slowCache) Set(ctx context.Context, _ string, _ string, _ []float32, _ string, _ *storage.CachedResult, _ time.Duration) error {
	s.once.Do(func() { close(s.setStarted) })
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	s.onceFin.Do(func() { close(s.setFinished) })
	return nil
}

// TestCacheWarmOnMiss_AsyncReturnsBeforeSet — Round-9 Task 9.
// When CacheWarmOnMiss=true the handler must return the HTTP
// response before the cache write completes. We assert that the
// in-process retrieval finished while the cache.Set is still
// blocked.
func TestCacheWarmOnMiss_AsyncReturnsBeforeSet(t *testing.T) {
	t.Parallel()
	cache := newSlowCache(500 * time.Millisecond)
	vs := &slowVectorStore{hits: []storage.QdrantHit{
		{ID: "doc:b1", Score: 0.5, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:     vs,
		Embedder:        &fakeEmbedder{vec: []float32{1}},
		Cache:           cache,
		CacheWarmOnMiss: true,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	start := time.Now()
	resp := h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{
		Requests: []retrieval.RetrieveRequest{{Query: "q1"}},
	})
	elapsed := time.Since(start)
	if len(resp.Results) != 1 || resp.Results[0].Response == nil {
		t.Fatalf("expected 1 result; got %+v", resp)
	}
	// Sanity: the cache.Set must have STARTED so we know the
	// async path was exercised.
	select {
	case <-cache.setStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("cache.Set never started — warm-on-miss not exercised")
	}
	// The retrieval should have returned well before the
	// cache.Set completed (500ms delay).
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("retrieval blocked on cache.Set; elapsed=%v (expected ≪ 500ms)", elapsed)
	}
	// And the cache write does eventually complete in the
	// background.
	select {
	case <-cache.setFinished:
	case <-time.After(2 * time.Second):
		t.Fatalf("background cache.Set never finished")
	}
}

// TestCacheWarmOnMiss_SyncByDefault confirms the sync path
// (CacheWarmOnMiss=false) still waits for the cache write.
func TestCacheWarmOnMiss_SyncByDefault(t *testing.T) {
	t.Parallel()
	cache := newSlowCache(200 * time.Millisecond)
	vs := &slowVectorStore{hits: []storage.QdrantHit{
		{ID: "doc:b1", Score: 0.5, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		Cache:       cache,
		// CacheWarmOnMiss left false → sync path.
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	start := time.Now()
	_ = h.RunBatch(context.Background(), "tenant-a", retrieval.BatchRequest{
		Requests: []retrieval.RetrieveRequest{{Query: "q1"}},
	})
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("sync path returned too fast; elapsed=%v (expected ≥ ~200ms)", elapsed)
	}
}

// trackingDiversifier records every Diversify invocation so the
// Round-11 Task 6 batch-handler test can assert each sub-request
// actually had its Diversity (lambda) threaded into the
// post-rerank pipeline.
type trackingDiversifier struct {
	mu       sync.Mutex
	calls    []float32
	delegate retrieval.Diversifier
}

func (t *trackingDiversifier) Diversify(ctx context.Context, matches []*retrieval.Match, lambda float32, topK int) []*retrieval.Match {
	t.mu.Lock()
	t.calls = append(t.calls, lambda)
	t.mu.Unlock()
	if t.delegate != nil {
		return t.delegate.Diversify(ctx, matches, lambda, topK)
	}
	return matches
}

func (t *trackingDiversifier) Calls() []float32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]float32, len(t.calls))
	copy(out, t.calls)
	return out
}

// TestBatch_DiversityFieldThreadedToSubRequests — Round-11 Task 6.
//
// Confirms that BatchRequest.Requests[i].Diversity is honoured per
// slot. Each sub-request is dispatched through Handler.RunBatch ->
// runOne -> RetrieveWithSnapshotCached -> runPipelineFromVec, which
// invokes the configured Diversifier when Diversity>0. The tracking
// diversifier records the lambda passed to it; we assert each
// non-zero sub-request appears in the call log.
func TestBatch_DiversityFieldThreadedToSubRequests(t *testing.T) {
	t.Parallel()
	td := &trackingDiversifier{}
	vs := &slowVectorStore{hits: []storage.QdrantHit{
		{ID: "a:b:1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
		{ID: "a:b:2", Score: 0.8, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
		Diversifier: td,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")

	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "q1", Diversity: 0.7},
		{Query: "q2", Diversity: 0.3},
		{Query: "q3"}, // Diversity=0 — Diversifier MUST NOT be invoked.
	}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}

	got := td.Calls()
	// Diversifier invoked exactly twice (for the two non-zero
	// sub-requests) with the supplied lambdas. Order is unstable
	// because the batch fans out concurrently — assert on the
	// set, not the slice.
	wantSet := map[float32]bool{0.7: false, 0.3: false}
	for _, v := range got {
		if _, ok := wantSet[v]; !ok {
			t.Fatalf("unexpected lambda observed: %v (got=%v)", v, got)
		}
		wantSet[v] = true
	}
	if len(got) != 2 {
		t.Fatalf("diversifier calls=%v want exactly 2 (one per non-zero Diversity slot)", got)
	}
	for k, seen := range wantSet {
		if !seen {
			t.Fatalf("lambda %v missing from diversifier calls=%v", k, got)
		}
	}
}

// TestBatch_TraceIDEcho_Round13Task3 — Round-13 Task 3.
//
// With a span recorder installed, the batch endpoint must wrap
// the entire fan-out in a parent "retrieval.batch" span and emit
// N child "retrieval.batch.sub" spans. The HTTP response echoes
// the parent trace id back to the caller in BatchResponse.TraceID.
//
// Runs serially because installTracer mutates the process-global
// otel TracerProvider; other parallel batch tests would otherwise
// emit "retrieval.batch" spans into this recorder and overwrite
// the parent trace id we're trying to verify.
func TestBatch_TraceIDEcho_Round13Task3(t *testing.T) {
	rec := installTracer(t)
	vs := &slowVectorStore{hits: []storage.QdrantHit{{ID: "a:b:c", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: &fakeEmbedder{vec: []float32{1, 2}}})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "q1"}, {Query: "q2"},
	}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got retrieval.BatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TraceID == "" {
		t.Fatalf("response.trace_id empty; want parent span id")
	}
	spans := rec.Ended()
	var parent string
	var subCount int
	for _, s := range spans {
		switch s.Name() {
		case "retrieval.batch":
			parent = s.SpanContext().TraceID().String()
		case "retrieval.batch.sub":
			subCount++
		}
	}
	if parent == "" {
		t.Fatal("retrieval.batch parent span missing")
	}
	if parent != got.TraceID {
		t.Fatalf("response.trace_id=%q parent span trace=%q", got.TraceID, parent)
	}
	if subCount != 2 {
		t.Fatalf("expected 2 retrieval.batch.sub spans, got %d", subCount)
	}
}
