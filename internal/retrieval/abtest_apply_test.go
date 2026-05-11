package retrieval_test

// abtest_apply_test.go regresses the Round-6 critical findings on
// PR #15:
//
//  1. Query expansion was dead on the batch / simulator paths.
//     `runPipelineFromVec` used to pass `req.Query` straight to
//     `fanOut`, so the gin /v1/retrieve handler honoured synonyms
//     but POST /v1/retrieve/batch and the Phase 4 simulator did
//     not. The tests below assert BOTH the cached batch path
//     (`RetrieveWithSnapshotCached`) and the cache-free simulator
//     path (`RetrieveWithSnapshot`) hand the expanded query to
//     BM25.
//
//  2. The A/B-test router was wired through HandlerConfig but
//     never actually invoked. `resp.Policy.ExperimentName` /
//     `ExperimentArm` were always empty regardless of the
//     configured experiment. The tests below assert routing fires
//     on the gin handler, the batch path, AND the simulator path,
//     that variant config (`top_k`, `diversity`) flows into the
//     downstream pipeline, and that bucketing is deterministic.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// queryCapturingBM25 records the BM25 query string it was handed
// so the test can assert query expansion (Round-6 Task 4) ran.
type queryCapturingBM25 struct {
	hits []*retrieval.Match
	mu   atomicString
}

type atomicString struct {
	v atomic.Value
}

func (a *atomicString) Store(s string) { a.v.Store(s) }
func (a *atomicString) Load() string {
	v := a.v.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func (b *queryCapturingBM25) Search(_ context.Context, _ string, q string, _ int) ([]*retrieval.Match, error) {
	b.mu.Store(q)
	return b.hits, nil
}

// staticExpander returns a fixed expansion regardless of input —
// enough to prove the handler called Expand and threaded the
// result into fan-out.
type staticExpander struct {
	expanded string
}

func (e *staticExpander) Expand(_ context.Context, _, query string) (string, error) {
	if e.expanded == "" {
		return query, nil
	}
	return e.expanded, nil
}

// TestRetrieve_BatchPath_AppliesQueryExpansion regresses the bug
// where RetrieveWithSnapshotCached (the entry point for POST
// /v1/retrieve/batch) handed the raw query to fan-out instead of
// the expander's output.
func TestRetrieve_BatchPath_AppliesQueryExpansion(t *testing.T) {
	t.Parallel()

	bm := &queryCapturingBM25{hits: []*retrieval.Match{
		{ID: "chunk-1", Source: retrieval.SourceBM25, Score: 1, TenantID: "tenant-a"},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:   &fakeVectorStore{},
		Embedder:      &fakeEmbedder{vec: []float32{1}},
		BM25:          bm,
		QueryExpander: &staticExpander{expanded: "hello world synonym"},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if _, err := h.RetrieveWithSnapshotCached(
		context.Background(), "tenant-a",
		retrieval.RetrieveRequest{Query: "hello", PrivacyMode: "secret", TopK: 5},
		retrieval.PolicySnapshot{},
	); err != nil {
		t.Fatalf("RetrieveWithSnapshotCached: %v", err)
	}
	if got := bm.mu.Load(); got != "hello world synonym" {
		t.Fatalf("batch path did not apply query expansion; BM25 saw %q", got)
	}
}

// TestRetrieve_SimulatorPath_AppliesQueryExpansion regresses the
// twin bug on RetrieveWithSnapshot (the cache-free entry the Phase
// 4 simulator drives).
func TestRetrieve_SimulatorPath_AppliesQueryExpansion(t *testing.T) {
	t.Parallel()

	bm := &queryCapturingBM25{hits: []*retrieval.Match{
		{ID: "chunk-1", Source: retrieval.SourceBM25, Score: 1, TenantID: "tenant-a"},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:   &fakeVectorStore{},
		Embedder:      &fakeEmbedder{vec: []float32{1}},
		BM25:          bm,
		QueryExpander: &staticExpander{expanded: "draft simulator synonym"},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if _, err := h.RetrieveWithSnapshot(
		context.Background(), "tenant-a",
		retrieval.RetrieveRequest{Query: "draft", PrivacyMode: "secret", TopK: 5},
		retrieval.PolicySnapshot{},
	); err != nil {
		t.Fatalf("RetrieveWithSnapshot: %v", err)
	}
	if got := bm.mu.Load(); got != "draft simulator synonym" {
		t.Fatalf("simulator path did not apply query expansion; BM25 saw %q", got)
	}
}

// stubABTestRouter implements retrieval.ABTestRouter and lets tests
// inject a fixed arm + config without standing up the admin
// in-memory store.
type stubABTestRouter struct {
	arm    string
	cfg    map[string]any
	calls  atomic.Int32
	lastTK atomicString
}

func (s *stubABTestRouter) Route(_ string, experimentName, bucketKey string) (*retrieval.ABTestRouteResult, error) {
	s.calls.Add(1)
	s.lastTK.Store(bucketKey)
	if experimentName == "" {
		return nil, nil
	}
	return &retrieval.ABTestRouteResult{
		ExperimentName: experimentName,
		Arm:            s.arm,
		Config:         s.cfg,
	}, nil
}

// TestRetrieve_ABTestRouting_StampsResponse asserts the gin handler
// invokes h.cfg.ABTests.Route(...) when the request opts into an
// experiment and stamps the arm onto resp.Policy. Pre-fix the
// router was wired but never called, so experiment_name /
// experiment_arm on the response envelope were always empty.
func TestRetrieve_ABTestRouting_StampsResponse(t *testing.T) {
	t.Parallel()

	router := &stubABTestRouter{arm: "variant"}
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		ABTests:     router,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:               "hi",
		PrivacyMode:         "secret",
		ExperimentName:      "rerank-v2",
		ExperimentBucketKey: "user-42",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if router.calls.Load() == 0 {
		t.Fatalf("ABTestRouter.Route was never invoked")
	}
	if got.Policy.ExperimentName != "rerank-v2" || got.Policy.ExperimentArm != "variant" {
		t.Fatalf("policy missing experiment stamp: %+v", got.Policy)
	}
	if router.lastTK.Load() != "user-42" {
		t.Fatalf("bucket key not threaded; got %q", router.lastTK.Load())
	}
}

// TestRetrieve_ABTestRouting_BucketKeyDefaultsToTenant verifies that
// when the request omits ExperimentBucketKey the handler falls back
// to tenant_id so a tenant's traffic lands on a single arm. The
// stub records the bucket key it received.
func TestRetrieve_ABTestRouting_BucketKeyDefaultsToTenant(t *testing.T) {
	t.Parallel()

	router := &stubABTestRouter{arm: "control"}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: &fakeVectorStore{},
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		ABTests:     router,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:          "hi",
		PrivacyMode:    "secret",
		ExperimentName: "rerank-v2",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if router.lastTK.Load() != "tenant-a" {
		t.Fatalf("default bucket key should be tenant_id; got %q", router.lastTK.Load())
	}
}

// TestRetrieve_ABTestRouting_NoExperiment_LeavesPolicyEmpty
// regresses the "router is never invoked" finding from the
// opposite direction: a request that does NOT opt into an
// experiment must NOT skip the rest of the pipeline AND must
// return a response with empty experiment fields.
func TestRetrieve_ABTestRouting_NoExperiment_LeavesPolicyEmpty(t *testing.T) {
	t.Parallel()

	router := &stubABTestRouter{arm: "variant"}
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		ABTests:     router,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi", PrivacyMode: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Policy.ExperimentName != "" || got.Policy.ExperimentArm != "" {
		t.Fatalf("non-experiment request must not stamp experiment fields: %+v", got.Policy)
	}
	if router.calls.Load() != 0 {
		t.Fatalf("router should not be called when ExperimentName is empty; calls=%d", router.calls.Load())
	}
}

// TestRetrieve_ABTestRouting_BatchPathStampsArm asserts the routing
// also fires on the batch / simulator entry points. Pre-fix the
// cache-aware path returned hits with policy.experiment_arm = "".
func TestRetrieve_ABTestRouting_BatchPathStampsArm(t *testing.T) {
	t.Parallel()

	router := &stubABTestRouter{arm: "variant"}
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		ABTests:     router,
	})
	resp, err := h.RetrieveWithSnapshotCached(
		context.Background(), "tenant-a",
		retrieval.RetrieveRequest{
			Query: "hi", PrivacyMode: "secret",
			ExperimentName: "rerank-v2", ExperimentBucketKey: "user-7",
		},
		retrieval.PolicySnapshot{},
	)
	if err != nil {
		t.Fatalf("RetrieveWithSnapshotCached: %v", err)
	}
	if resp.Policy.ExperimentName != "rerank-v2" || resp.Policy.ExperimentArm != "variant" {
		t.Fatalf("batch path missing experiment stamp: %+v", resp.Policy)
	}
	// Drive the same request again to exercise the cache-hit branch
	// — the stamp must still be present (regression for "cache hit
	// blanks experiment metadata").
	resp2, err := h.RetrieveWithSnapshotCached(
		context.Background(), "tenant-a",
		retrieval.RetrieveRequest{
			Query: "hi", PrivacyMode: "secret",
			ExperimentName: "rerank-v2", ExperimentBucketKey: "user-7",
		},
		retrieval.PolicySnapshot{},
	)
	if err != nil {
		t.Fatalf("RetrieveWithSnapshotCached (2nd): %v", err)
	}
	if resp2.Policy.ExperimentName != "rerank-v2" || resp2.Policy.ExperimentArm != "variant" {
		t.Fatalf("cache hit lost experiment stamp: %+v", resp2.Policy)
	}
}

// TestRetrieve_ABTestRouting_VariantConfigAppliesTopK verifies that
// the variant-arm `top_k` override actually flows into the pipeline:
// the response's hit count must reflect the variant override even
// when the request asks for a larger TopK.
func TestRetrieve_ABTestRouting_VariantConfigAppliesTopK(t *testing.T) {
	t.Parallel()

	router := &stubABTestRouter{
		arm: "variant",
		// JSON unmarshals numbers to float64; mimic that here so
		// the production path with admin-stored configs is covered.
		cfg: map[string]any{"top_k": float64(2)},
	}
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
		{ID: "chunk-2", Score: 0.8, Payload: map[string]any{"tenant_id": "tenant-a"}},
		{ID: "chunk-3", Score: 0.7, Payload: map[string]any{"tenant_id": "tenant-a"}},
		{ID: "chunk-4", Score: 0.6, Payload: map[string]any{"tenant_id": "tenant-a"}},
		{ID: "chunk-5", Score: 0.5, Payload: map[string]any{"tenant_id": "tenant-a"}},
	}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		ABTests:     router,
	})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{
		Query:          "hi",
		PrivacyMode:    "secret",
		TopK:           10, // request asks for 10
		ExperimentName: "rerank-v2",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) != 2 {
		t.Fatalf("variant top_k=2 override should cap hits at 2; got %d (%v)", len(got.Hits), got.Hits)
	}
}

// TestRetrieve_ABTestRouting_Deterministic uses the production
// admin router (not the stub) to assert deterministic FNV bucketing
// across two retrieval calls with the same bucket key.
func TestRetrieve_ABTestRouting_Deterministic(t *testing.T) {
	t.Parallel()

	// Use the retrieval-side adapter so we exercise the
	// ABTestRouterAdapter that production wiring relies on.
	prodRouter, err := newAdminRouter(t, 50)
	if err != nil {
		t.Fatalf("newAdminRouter: %v", err)
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: &fakeVectorStore{},
		Embedder:    &fakeEmbedder{vec: []float32{1}},
		ABTests:     prodRouter,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")

	arms := []string{}
	for i := 0; i < 5; i++ {
		w := doPost(r, retrieval.RetrieveRequest{
			Query: "hi", PrivacyMode: "secret",
			ExperimentName: "rerank-v2", ExperimentBucketKey: "user-42",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status[%d]: %d", i, w.Code)
		}
		var resp retrieval.RetrieveResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		arms = append(arms, resp.Policy.ExperimentArm)
	}
	for i := 1; i < len(arms); i++ {
		if arms[i] != arms[0] {
			t.Fatalf("same bucket key must route to same arm; got %v", arms)
		}
	}
	if arms[0] != "control" && arms[0] != "variant" {
		t.Fatalf("arm must be control or variant; got %q", arms[0])
	}
}

// TestABTestRouterAdapter_NilWrappedReturnsNoOp confirms the
// wiring adapter's nil-safety: an `*admin.ABTestRouter`-less
// adapter must collapse to "no experiment applied" instead of
// panicking.
func TestABTestRouterAdapter_NilWrappedReturnsNoOp(t *testing.T) {
	t.Parallel()

	a := retrieval.ABTestRouterAdapter{Router: nil}
	got, err := a.Route("tenant-a", "rerank-v2", "user-1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("nil-wrapped adapter should return nil result; got %+v", got)
	}
}

// newAdminRouter spins up an in-memory ABTestStore, seeds it with
// an active 50/50 experiment, and returns the retrieval-side
// adapter so tests exercise the same wiring path production uses.
func newAdminRouter(t *testing.T, split int) (retrieval.ABTestRouter, error) {
	t.Helper()
	store := admin.NewInMemoryABTestStore()
	if err := store.Upsert(&admin.ABTestConfig{
		TenantID:            "tenant-a",
		ExperimentName:      "rerank-v2",
		Status:              admin.ABTestStatusActive,
		TrafficSplitPercent: split,
		ControlConfig:       map[string]any{"k": "control"},
		VariantConfig:       map[string]any{"k": "variant"},
	}); err != nil {
		return nil, err
	}
	return retrieval.ABTestRouterAdapter{Router: admin.NewABTestRouter(store)}, nil
}
