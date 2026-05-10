package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

type fakeVectorStore struct {
	mu        sync.Mutex
	calls     int
	tenant    string
	hits      []storage.QdrantHit
	searchErr error
	gotOpts   storage.SearchOpts
}

func (f *fakeVectorStore) Search(_ context.Context, tenantID string, _ []float32, opts storage.SearchOpts) ([]storage.QdrantHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.tenant = tenantID
	f.gotOpts = opts
	if f.searchErr != nil {
		return nil, f.searchErr
	}

	return f.hits, nil
}

type fakeEmbedder struct {
	vec []float32
	err error
}

func (f *fakeEmbedder) EmbedQuery(_ context.Context, _ string, _ string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.vec, nil
}

func newRouter(t *testing.T, h *retrieval.Handler, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if tenantID != "" {
			c.Set(audit.TenantContextKey, tenantID)
		}
		c.Next()
	})
	h.Register(&r.RouterGroup)

	return r
}

func doPost(r *gin.Engine, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	return w
}

func TestRetrieve_HappyPath(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "a:b:c", Score: 0.9, Payload: map[string]any{
				"tenant_id":     "tenant-a",
				"document_id":   "doc-1",
				"block_id":      "b1",
				"title":         "hello",
				"text":          "world",
				"privacy_label": "internal",
				"source_id":     "src-1",
				"connector":     "google_drive",
				"extra":         "xyz",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("hits: %d", len(got.Hits))
	}
	hit := got.Hits[0]
	if hit.PrivacyLabel != "internal" {
		t.Fatalf("privacy_label: %q", hit.PrivacyLabel)
	}
	if hit.Title != "hello" || hit.Text != "world" {
		t.Fatalf("hit: %+v", hit)
	}
	if hit.Metadata["extra"] != "xyz" {
		t.Fatalf("metadata: %+v", hit.Metadata)
	}
	if vs.tenant != "tenant-a" {
		t.Fatalf("vs.tenant: %q", vs.tenant)
	}
	if got.Policy.PrivacyMode == "" {
		t.Fatalf("policy.privacy_mode should be populated")
	}
	if len(got.Policy.Applied) == 0 {
		t.Fatalf("policy.applied should list contributing backends")
	}
}

func TestRetrieve_TenantIsolation(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})

	// Tenant-a's request must reach VectorStore as tenant-a, not
	// whatever the request body claims.
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if vs.tenant != "tenant-a" {
		t.Fatalf("tenant: %q", vs.tenant)
	}
}

func TestRetrieve_RejectsUnauthenticated(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})

	r := newRouter(t, h, "")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
	if vs.calls != 0 {
		t.Fatalf("VectorStore must not be called: %d", vs.calls)
	}
}

func TestRetrieve_BadRequest(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	r := newRouter(t, h, "tenant-a")

	for _, body := range []any{
		retrieval.RetrieveRequest{}, // missing query
		retrieval.RetrieveRequest{Query: "hi", TopK: -1},
	} {
		w := doPost(r, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%v status=%d", body, w.Code)
		}
	}
}

func TestRetrieve_EmptyResults(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{hits: nil}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) != 0 {
		t.Fatalf("hits: %d", len(got.Hits))
	}
}

func TestRetrieve_EmbedderFailureReturns500(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{err: errors.New("boom")}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", w.Code)
	}
	if vs.calls != 0 {
		t.Fatalf("VectorStore must not be called when embed fails: %d", vs.calls)
	}
}

func TestRetrieve_VectorStoreFailureSurfacesAsDegraded(t *testing.T) {
	t.Parallel()

	// Phase 3 fan-out behaviour: a backend failure should NOT take
	// retrieval offline. The handler returns 200 with empty hits and
	// policy.degraded listing the failed backend so the client can
	// render a "partial result" badge.
	vs := &fakeVectorStore{searchErr: errors.New("qdrant down")}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Policy.Degraded) != 1 || got.Policy.Degraded[0] != retrieval.SourceVector {
		t.Fatalf("policy.degraded: %v", got.Policy.Degraded)
	}
	if len(got.Hits) != 0 {
		t.Fatalf("expected 0 hits on full failure, got %d", len(got.Hits))
	}
}

func TestRetrieve_TopKDefaultsAndCaps(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb, DefaultTopK: 5, MaxTopK: 8})
	r := newRouter(t, h, "tenant-a")

	// Default
	doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if vs.gotOpts.Limit != 5 {
		t.Fatalf("default limit: %d", vs.gotOpts.Limit)
	}
	// Cap
	doPost(r, retrieval.RetrieveRequest{Query: "hi", TopK: 100})
	if vs.gotOpts.Limit != 8 {
		t.Fatalf("capped limit: %d", vs.gotOpts.Limit)
	}
}

func TestRetrieve_FiltersForwarded(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	r := newRouter(t, h, "tenant-a")

	doPost(r, retrieval.RetrieveRequest{Query: "hi", Sources: []string{"src-1"}, Documents: []string{"d-1", "d-2"}})
	if vs.gotOpts.Filter == nil {
		t.Fatal("expected filter to be set")
	}
}

func TestRetrieve_NewHandler_Validation(t *testing.T) {
	t.Parallel()

	if _, err := retrieval.NewHandler(retrieval.HandlerConfig{Embedder: &fakeEmbedder{}}); err == nil {
		t.Fatal("expected error: missing VectorStore")
	}
	if _, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: &fakeVectorStore{}}); err == nil {
		t.Fatal("expected error: missing Embedder")
	}
}

// TestRetrieve_Timings_PopulatedFromBackends verifies the Phase 1
// Task 19 RetrieveResponse.Timings field is populated from each
// configured backend's wall-clock duration. Vector + BM25 are
// wired so vector_ms and bm25_ms must be non-negative; merge_ms
// and rerank_ms always run so they too must be non-negative.
func TestRetrieve_Timings_PopulatedFromBackends(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "a:b:c", Score: 0.9, Payload: map[string]any{
				"tenant_id":     "tenant-a",
				"document_id":   "doc-1",
				"title":         "t",
				"text":          "x",
				"privacy_label": "internal",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{0.5}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}

	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// vector_ms and graph/memory/bm25 are 0 when not wired — but
	// merge_ms and rerank_ms always run.
	if got.Timings.VectorMs < 0 {
		t.Errorf("vector_ms < 0: %d", got.Timings.VectorMs)
	}
	if got.Timings.MergeMs < 0 || got.Timings.RerankMs < 0 {
		t.Errorf("merge/rerank timings negative: merge=%d rerank=%d",
			got.Timings.MergeMs, got.Timings.RerankMs)
	}
}

// TestRetrieve_Timings_JSONShape verifies the Timings field is
// serialised under a stable JSON shape so dashboards and clients
// can rely on it without unmarshalling the full RetrieveResponse.
func TestRetrieve_Timings_JSONShape(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{}
	emb := &fakeEmbedder{vec: []float32{1}}
	h, _ := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	r := newRouter(t, h, "tenant-a")
	w := doPost(r, retrieval.RetrieveRequest{Query: "hi"})

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["timings"]; !ok {
		t.Fatalf("response is missing timings field; body=%s", w.Body.String())
	}
	var timings map[string]int64
	if err := json.Unmarshal(raw["timings"], &timings); err != nil {
		t.Fatalf("decode timings: %v", err)
	}
	for _, k := range []string{"vector_ms", "bm25_ms", "graph_ms", "memory_ms", "merge_ms", "rerank_ms"} {
		if _, ok := timings[k]; !ok {
			t.Errorf("timings field %q missing", k)
		}
	}
}
