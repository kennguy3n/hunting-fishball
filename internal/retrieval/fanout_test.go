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

	"github.com/kennguy3n/hunting-fishball/internal/policy"
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

// stubResolver returns a fixed PolicySnapshot for every request so
// the cache-hit + ACL/recipient gate tests can drive deterministic
// snapshots into the handler.
type stubResolver struct {
	snap retrieval.PolicySnapshot
}

func (s stubResolver) Resolve(_ context.Context, _, _ string) (retrieval.PolicySnapshot, error) {
	return s.snap, nil
}

// TestFanout_CacheKey_DiffersBySkillID exercises the regression where
// scopeHashFor omitted SkillID — two requests with the same query /
// channel / topK but different SkillIDs would otherwise share a cache
// slot, letting a recipient-denied skill be served a cached response
// originally produced for an allowed skill.
func TestFanout_CacheKey_DiffersBySkillID(t *testing.T) {
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

	w1 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "secret", SkillID: "summarizer"})
	if w1.Code != http.StatusOK {
		t.Fatalf("skill=summarizer status: %d", w1.Code)
	}
	w2 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "secret", SkillID: "exfil"})
	if w2.Code != http.StatusOK {
		t.Fatalf("skill=exfil status: %d", w2.Code)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.setHashes) != 2 {
		t.Fatalf("setHashes: %v", cache.setHashes)
	}
	if cache.setHashes[0] == cache.setHashes[1] {
		t.Fatalf("scope hash must differ by SkillID; both = %q", cache.setHashes[0])
	}
	if len(cache.getHashes) != 2 || cache.getHashes[0] == cache.getHashes[1] {
		t.Fatalf("Get scope hash must differ by SkillID: %v", cache.getHashes)
	}
}

// TestFanout_CacheKey_DiffersByDiversity exercises the Round-6
// regression where scopeHashFor omitted Diversity. Two requests
// with the same query / channel / topK / skill but different
// diversity lambdas must produce distinct scope hashes — otherwise
// the MMR-re-ranked diverse response would be silently served from
// (or to) a pure-relevance cache slot.
func TestFanout_CacheKey_DiffersByDiversity(t *testing.T) {
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

	w1 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "secret", Diversity: 0})
	if w1.Code != http.StatusOK {
		t.Fatalf("diversity=0 status: %d", w1.Code)
	}
	w2 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "secret", Diversity: 0.7})
	if w2.Code != http.StatusOK {
		t.Fatalf("diversity=0.7 status: %d", w2.Code)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.setHashes) != 2 {
		t.Fatalf("setHashes: %v", cache.setHashes)
	}
	if cache.setHashes[0] == cache.setHashes[1] {
		t.Fatalf("scope hash must differ by Diversity; both = %q", cache.setHashes[0])
	}
	if len(cache.getHashes) != 2 || cache.getHashes[0] == cache.getHashes[1] {
		t.Fatalf("Get scope hash must differ by Diversity: %v", cache.getHashes)
	}
}

// TestFanout_CacheKey_DiffersByExperiment exercises the regression
// where scopeHashFor omitted ExperimentName / ExperimentBucketKey.
// Two requests with identical inputs except experiment routing must
// land in distinct cache slots — otherwise the variant response
// (different reranker / fan-out weights) would be served from the
// control cache entry, contaminating A/B measurements.
func TestFanout_CacheKey_DiffersByExperiment(t *testing.T) {
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
		t.Fatalf("no-experiment status: %d", w1.Code)
	}
	w2 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "secret", ExperimentName: "rerank-v2"})
	if w2.Code != http.StatusOK {
		t.Fatalf("experiment status: %d", w2.Code)
	}
	w3 := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "secret", ExperimentName: "rerank-v2", ExperimentBucketKey: "user-42"})
	if w3.Code != http.StatusOK {
		t.Fatalf("bucket-key status: %d", w3.Code)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.setHashes) != 3 {
		t.Fatalf("setHashes: %v", cache.setHashes)
	}
	if cache.setHashes[0] == cache.setHashes[1] {
		t.Fatalf("scope hash must differ by ExperimentName; both = %q", cache.setHashes[0])
	}
	if cache.setHashes[1] == cache.setHashes[2] {
		t.Fatalf("scope hash must differ by ExperimentBucketKey; both = %q", cache.setHashes[1])
	}
}

// TestFanout_CacheHit_AppliesACL verifies the cache-hit short-circuit
// re-applies the Phase 4 ACL gate to cached chunks. A policy change
// landed between the cache write and read MUST drop denied chunks
// instead of serving the stale entry.
func TestFanout_CacheHit_AppliesACL(t *testing.T) {
	t.Parallel()

	cached := &storage.CachedResult{
		Hits: []storage.CachedHit{
			{ID: "ok-1", TenantID: "tenant-a", SourceID: "s1", Metadata: map[string]any{"path": "docs/intro.md"}},
			{ID: "bad-1", TenantID: "tenant-a", SourceID: "s1", Metadata: map[string]any{"path": "secrets/keys.txt"}},
			{ID: "ok-2", TenantID: "tenant-a", SourceID: "s1", Metadata: map[string]any{"path": "docs/howto.md"}},
		},
	}
	cache := &fakeCache{getValue: cached}
	resolver := stubResolver{snap: retrieval.PolicySnapshot{
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "secrets/*", Action: policy.ACLActionDeny},
		}},
	}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:    &fakeVectorStore{},
		Embedder:       &fakeEmbedder{vec: []float32{1}},
		Cache:          cache,
		PolicyResolver: resolver,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 10, PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Policy.CacheHit {
		t.Fatalf("expected cache hit")
	}
	if len(got.Hits) != 2 {
		t.Fatalf("ACL must drop the secrets hit on cache-hit path: %v", topNIDs(got.Hits))
	}
	for _, hit := range got.Hits {
		if hit.ID == "bad-1" {
			t.Fatalf("denied chunk leaked through cache-hit path: %v", topNIDs(got.Hits))
		}
	}
	if got.Policy.BlockedCount != 1 {
		t.Fatalf("BlockedCount must reflect cache-hit ACL drop: %d", got.Policy.BlockedCount)
	}
}

// TestFanout_CacheHit_AppliesRecipientPolicy verifies the cache-hit
// short-circuit honors recipient-policy denials. A skill denied by
// the recipient policy must receive zero hits even when the cache
// has a populated entry for the request scope.
func TestFanout_CacheHit_AppliesRecipientPolicy(t *testing.T) {
	t.Parallel()

	cached := &storage.CachedResult{
		Hits: []storage.CachedHit{
			{ID: "c-1", TenantID: "tenant-a"},
			{ID: "c-2", TenantID: "tenant-a"},
		},
	}
	cache := &fakeCache{getValue: cached}
	resolver := stubResolver{snap: retrieval.PolicySnapshot{
		Recipient: &policy.RecipientPolicy{
			Rules: []policy.RecipientRule{
				{SkillID: "exfil", Action: policy.ACLActionDeny},
			},
			DefaultAllow: true,
		},
	}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:    &fakeVectorStore{},
		Embedder:       &fakeEmbedder{vec: []float32{1}},
		Cache:          cache,
		PolicyResolver: resolver,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 10, PrivacyMode: "secret", SkillID: "exfil"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Policy.CacheHit {
		t.Fatalf("expected cache hit")
	}
	if len(got.Hits) != 0 {
		t.Fatalf("recipient deny must drop every cached hit: %v", topNIDs(got.Hits))
	}
	if got.Policy.BlockedCount != 2 {
		t.Fatalf("BlockedCount must reflect recipient denial: %d", got.Policy.BlockedCount)
	}
}

// TestFanout_CacheKey_DiffersByEffectiveMode exercises the regression
// where scopeHashFor hashed req.PrivacyMode (raw request field) but
// the handler may override it with snapshot.EffectiveMode from the
// PolicyResolver. Two requests with identical req.PrivacyMode but
// different resolver-determined effective modes must compute distinct
// cache keys — otherwise an admin tightening the tenant policy from
// "remote" to "local-only" leaves stale cache entries serving the
// more permissive mode until the TTL expires.
func TestFanout_CacheKey_DiffersByEffectiveMode(t *testing.T) {
	t.Parallel()

	cacheA := &fakeCache{}
	vsA := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	hA, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:    vsA,
		Embedder:       &fakeEmbedder{vec: []float32{1}},
		Cache:          cacheA,
		PolicyResolver: stubResolver{snap: retrieval.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}},
	})
	rA := newRouter(t, hA, "tenant-a")

	cacheB := &fakeCache{}
	vsB := &fakeVectorStore{
		hits: []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}}},
	}
	hB, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:    vsB,
		Embedder:       &fakeEmbedder{vec: []float32{1}},
		Cache:          cacheB,
		PolicyResolver: stubResolver{snap: retrieval.PolicySnapshot{EffectiveMode: policy.PrivacyModeLocalOnly}},
	})
	rB := newRouter(t, hB, "tenant-a")

	// Identical req.PrivacyMode, different resolver-determined modes.
	wA := doPost(rA, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "remote"})
	if wA.Code != http.StatusOK {
		t.Fatalf("remote-mode status: %d body=%s", wA.Code, wA.Body.String())
	}
	wB := doPost(rB, retrieval.RetrieveRequest{Query: "hi", TopK: 5, PrivacyMode: "remote"})
	if wB.Code != http.StatusOK {
		t.Fatalf("local-only status: %d body=%s", wB.Code, wB.Body.String())
	}

	cacheA.mu.Lock()
	defer cacheA.mu.Unlock()
	cacheB.mu.Lock()
	defer cacheB.mu.Unlock()
	if len(cacheA.setHashes) != 1 || len(cacheB.setHashes) != 1 {
		t.Fatalf("expected one Set per handler: A=%v B=%v", cacheA.setHashes, cacheB.setHashes)
	}
	if cacheA.setHashes[0] == cacheB.setHashes[0] {
		t.Fatalf("scope hash must differ by resolver-determined mode; both = %q", cacheA.setHashes[0])
	}
	if len(cacheA.getHashes) != 1 || len(cacheB.getHashes) != 1 {
		t.Fatalf("expected one Get per handler: A=%v B=%v", cacheA.getHashes, cacheB.getHashes)
	}
	if cacheA.getHashes[0] == cacheB.getHashes[0] {
		t.Fatalf("Get scope hash must differ by resolver-determined mode: %q vs %q", cacheA.getHashes[0], cacheB.getHashes[0])
	}
}

// TestFanout_CacheHit_AppliesPrivacyMode verifies the cache-hit
// short-circuit re-applies the privacy-label PolicyFilter using the
// resolved effective mode. A cache entry written under a permissive
// privacy mode must NOT serve chunks that exceed a subsequently
// tightened mode — even before the entry's scope-hash slot rolls
// over to a fresh entry under the new mode.
func TestFanout_CacheHit_AppliesPrivacyMode(t *testing.T) {
	t.Parallel()

	cached := &storage.CachedResult{
		Hits: []storage.CachedHit{
			{ID: "pub-1", TenantID: "tenant-a", PrivacyLabel: "public"},
			{ID: "sec-1", TenantID: "tenant-a", PrivacyLabel: "secret"},
			{ID: "pub-2", TenantID: "tenant-a", PrivacyLabel: "public"},
		},
	}
	cache := &fakeCache{getValue: cached}
	// Resolver pins the effective mode to "public" — only public-labelled
	// chunks may pass; the cached secret-labelled chunk must drop.
	resolver := stubResolver{snap: retrieval.PolicySnapshot{EffectiveMode: policy.PrivacyMode("public")}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:    &fakeVectorStore{},
		Embedder:       &fakeEmbedder{vec: []float32{1}},
		Cache:          cache,
		PolicyResolver: resolver,
	})
	r := newRouter(t, h, "tenant-a")
	// Caller asks for "secret" but the resolver tightens to "public".
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 10, PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Policy.CacheHit {
		t.Fatalf("expected cache hit")
	}
	if got.Policy.PrivacyMode != "public" {
		t.Fatalf("response must reflect resolved mode, got %q", got.Policy.PrivacyMode)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("privacy filter must drop secret-labelled chunk on cache-hit path: %v", topNIDs(got.Hits))
	}
	for _, hit := range got.Hits {
		if hit.ID == "sec-1" {
			t.Fatalf("secret chunk leaked through cache-hit privacy filter: %v", topNIDs(got.Hits))
		}
	}
	if got.Policy.BlockedCount != 1 {
		t.Fatalf("BlockedCount must reflect cache-hit privacy drop: %d", got.Policy.BlockedCount)
	}
}
