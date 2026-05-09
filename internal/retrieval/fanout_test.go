package retrieval_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

type fakeBM25 struct {
	hits []*retrieval.Match
	err  error

	calls int32
}

func (f *fakeBM25) Search(_ context.Context, _ string, _ string, _ int) ([]*retrieval.Match, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}

	return f.hits, nil
}

type fakeGraph struct {
	hits []*retrieval.Match
	err  error
}

func (f *fakeGraph) Traverse(_ context.Context, _ string, _ string, _ int) ([]*retrieval.Match, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.hits, nil
}

type fakeMemory struct {
	hits []*retrieval.Match
	err  error
}

func (f *fakeMemory) Search(_ context.Context, _ string, _ string, _ int) ([]*retrieval.Match, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.hits, nil
}

type slowBackend struct {
	delay time.Duration
	hits  []*retrieval.Match
}

func (s *slowBackend) Search(ctx context.Context, _ string, _ string, _ int) ([]*retrieval.Match, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return s.hits, nil
	}
}

type fakeCache struct {
	mu       sync.Mutex
	getValue *storage.CachedResult
	getErr   error
	setErr   error

	getCalls    int32
	setCalls    int32
	lastSet     *storage.CachedResult
	lastSetHash string
	getHashes   []string
	setHashes   []string
}

func (f *fakeCache) Get(_ context.Context, _ string, _ string, _ []float32, scopeHash string) (*storage.CachedResult, error) {
	atomic.AddInt32(&f.getCalls, 1)
	f.mu.Lock()
	f.getHashes = append(f.getHashes, scopeHash)
	f.mu.Unlock()

	return f.getValue, f.getErr
}

func (f *fakeCache) Set(_ context.Context, _ string, _ string, _ []float32, scopeHash string, r *storage.CachedResult, _ time.Duration) error {
	atomic.AddInt32(&f.setCalls, 1)
	f.mu.Lock()
	f.lastSet = r
	f.lastSetHash = scopeHash
	f.setHashes = append(f.setHashes, scopeHash)
	f.mu.Unlock()

	return f.setErr
}

func TestFanout_VectorAndBM25_FuseInRRF(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a", "title": "v"}},
			{ID: "chunk-2", Score: 0.8, Payload: map[string]any{"tenant_id": "tenant-a", "title": "v"}},
		},
	}
	bm := &fakeBM25{
		hits: []*retrieval.Match{
			{ID: "chunk-2", Source: retrieval.SourceBM25, Score: 5.0, TenantID: "tenant-a"},
			{ID: "chunk-3", Source: retrieval.SourceBM25, Score: 4.0, TenantID: "tenant-a"},
		},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
		BM25:        bm,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Hits) != 3 {
		t.Fatalf("hits: %d", len(got.Hits))
	}
	// chunk-2 appeared in both streams → highest RRF score → top.
	if got.Hits[0].ID != "chunk-2" {
		t.Fatalf("expected chunk-2 first, got %v", topNIDs(got.Hits))
	}
	sources := append([]string{}, got.Policy.Applied...)
	sort.Strings(sources)
	if len(sources) != 2 || sources[0] != retrieval.SourceBM25 || sources[1] != retrieval.SourceVector {
		t.Fatalf("policy.applied: %v", got.Policy.Applied)
	}
}

func TestFanout_GraphAndMemoryAreCalled(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	bm := &fakeBM25{}
	gr := &fakeGraph{hits: []*retrieval.Match{{ID: "node-1", Source: retrieval.SourceGraph, TenantID: "tenant-a"}}}
	mem := &fakeMemory{hits: []*retrieval.Match{{ID: "mem-1", Source: retrieval.SourceMemory, TenantID: "tenant-a"}}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		BM25:        bm,
		Graph:       gr,
		Memory:      mem,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	ids := topNIDs(got.Hits)
	sort.Strings(ids)
	wantIDs := []string{"mem-1", "node-1"}
	if len(ids) != len(wantIDs) || ids[0] != wantIDs[0] || ids[1] != wantIDs[1] {
		t.Fatalf("ids: %v", ids)
	}
}

func TestFanout_PartialFailureDegrades(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	bm := &fakeBM25{err: errors.New("bm25 down")}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		BM25:        bm,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) != 1 || got.Hits[0].ID != "chunk-1" {
		t.Fatalf("expected vector-only result: %+v", got.Hits)
	}
	if len(got.Policy.Degraded) != 1 || got.Policy.Degraded[0] != retrieval.SourceBM25 {
		t.Fatalf("policy.degraded: %v", got.Policy.Degraded)
	}
}

func TestFanout_PerBackendDeadlineDegrades(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	slow := &slowBackend{delay: 200 * time.Millisecond}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1}},
		BM25:               slow,
		PerBackendDeadline: 10 * time.Millisecond,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Policy.Degraded) != 1 || got.Policy.Degraded[0] != retrieval.SourceBM25 {
		t.Fatalf("policy.degraded: %v", got.Policy.Degraded)
	}
}

func TestFanout_PolicyFilterDropsAboveChannelMode(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "ok", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a", "privacy_label": "internal"}},
			{ID: "bad", Score: 0.8, Payload: map[string]any{"tenant_id": "tenant-a", "privacy_label": "secret"}},
		},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "internal"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) != 1 || got.Hits[0].ID != "ok" {
		t.Fatalf("hits: %v", topNIDs(got.Hits))
	}
	if got.Policy.BlockedCount != 1 {
		t.Fatalf("blocked: %d", got.Policy.BlockedCount)
	}
}

func TestFanout_CacheHit_ShortCircuitsFanout(t *testing.T) {
	t.Parallel()

	cached := &storage.CachedResult{
		Hits: []storage.CachedHit{{ID: "cached-1", Score: 0.99, TenantID: "tenant-a", Title: "from-cache"}},
	}
	cache := &fakeCache{getValue: cached}
	vs := &fakeVectorStore{}
	bm := &fakeBM25{}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		BM25:        bm,
		Cache:       cache,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Policy.CacheHit {
		t.Fatalf("policy.cache_hit should be true")
	}
	if len(got.Hits) != 1 || got.Hits[0].ID != "cached-1" {
		t.Fatalf("hits: %v", topNIDs(got.Hits))
	}
	if vs.calls != 0 || bm.calls != 0 {
		t.Fatalf("backends called despite cache hit: vs=%d bm=%d", vs.calls, bm.calls)
	}
}

func TestFanout_CacheMiss_PopulatesCache(t *testing.T) {
	t.Parallel()

	cache := &fakeCache{}
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		Cache:       cache,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if atomic.LoadInt32(&cache.getCalls) != 1 {
		t.Fatalf("cache.Get calls: %d", cache.getCalls)
	}
	if atomic.LoadInt32(&cache.setCalls) != 1 {
		t.Fatalf("cache.Set calls: %d", cache.setCalls)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.lastSet == nil || len(cache.lastSet.Hits) != 1 || cache.lastSet.Hits[0].ID != "chunk-1" {
		t.Fatalf("cache.lastSet: %+v", cache.lastSet)
	}
	if len(cache.lastSet.ChunkIDs) != 1 || cache.lastSet.ChunkIDs[0] != "chunk-1" {
		t.Fatalf("ChunkIDs: %v", cache.lastSet.ChunkIDs)
	}
}

func TestFanout_CacheGetError_FallsBackToFanout(t *testing.T) {
	t.Parallel()

	cache := &fakeCache{getErr: errors.New("redis down")}
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		Cache:       cache,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Policy.CacheHit {
		t.Fatalf("expected cache miss path")
	}
	if len(got.Hits) != 1 || got.Hits[0].ID != "chunk-1" {
		t.Fatalf("hits: %v", topNIDs(got.Hits))
	}
}

// TestFanout_CacheKey_DiffersByTopK exercises the regression where
// scopeHashFor omitted topK, letting a topK=5 request poison or be
// served by a topK=100 cache entry. Two requests with identical
// inputs except topK must produce distinct scope hashes.
func TestFanout_CacheKey_DiffersByTopK(t *testing.T) {
	t.Parallel()

	cache := &fakeCache{}
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		Cache:       cache,
	})
	r := newRouter(t, h, "tenant-a")

	w1 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "secret"})
	if w1.Code != http.StatusOK {
		t.Fatalf("topK=5 status: %d", w1.Code)
	}
	w2 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 100, PrivacyMode: "secret"})
	if w2.Code != http.StatusOK {
		t.Fatalf("topK=100 status: %d", w2.Code)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.setHashes) != 2 {
		t.Fatalf("setHashes: %v", cache.setHashes)
	}
	if cache.setHashes[0] == cache.setHashes[1] {
		t.Fatalf("scope hash must differ by topK; both = %q", cache.setHashes[0])
	}
	if len(cache.getHashes) != 2 || cache.getHashes[0] == cache.getHashes[1] {
		t.Fatalf("Get scope hash must differ by topK: %v", cache.getHashes)
	}
}

// TestFanout_CacheHit_RespectsTopK exercises the defensive slice in
// responseFromCache: even if a stale entry from an older binary slips
// through with more hits than topK, we never return more than topK.
func TestFanout_CacheHit_RespectsTopK(t *testing.T) {
	t.Parallel()

	cached := &storage.CachedResult{
		Hits: []storage.CachedHit{
			{ID: "c-1", Score: 0.9, TenantID: "tenant-a"},
			{ID: "c-2", Score: 0.8, TenantID: "tenant-a"},
			{ID: "c-3", Score: 0.7, TenantID: "tenant-a"},
			{ID: "c-4", Score: 0.6, TenantID: "tenant-a"},
			{ID: "c-5", Score: 0.5, TenantID: "tenant-a"},
		},
	}
	cache := &fakeCache{getValue: cached}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: &fakeVectorStore{},
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		Cache:       cache,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 2, PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Policy.CacheHit {
		t.Fatalf("expected cache hit")
	}
	if len(got.Hits) != 2 {
		t.Fatalf("topK=2 must trim cached hits, got %d: %v", len(got.Hits), topNIDs(got.Hits))
	}
	if got.Hits[0].ID != "c-1" || got.Hits[1].ID != "c-2" {
		t.Fatalf("trim should preserve order: %v", topNIDs(got.Hits))
	}
}

func topNIDs(hits []retrieval.RetrieveHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}

	return out
}
