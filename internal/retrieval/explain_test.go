package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// stubBM25 implements retrieval.BM25Backend with a deterministic
// fixture so the merger and reranker have something to fuse with
// the vector backend.
type stubBM25 struct {
	matches []*retrieval.Match
}

func (s *stubBM25) Search(_ context.Context, _ string, _ string, _ int) ([]*retrieval.Match, error) {
	return s.matches, nil
}

func newExplainRouter(t *testing.T, h *retrieval.Handler, tenantID string, role admin.Role) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if tenantID != "" {
			c.Set(audit.TenantContextKey, tenantID)
		}
		if role != "" {
			c.Set(admin.RoleContextKey, role)
		}
		c.Next()
	})
	h.Register(&r.RouterGroup)
	return r
}

func postExplain(r *gin.Engine, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestExplain_PopulatedForAdmin(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "doc:b1", Score: 0.91, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
				"title": "T", "text": "x", "privacy_label": "internal",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	bm := &stubBM25{matches: []*retrieval.Match{{
		ID: "doc:b1", Source: retrieval.SourceBM25, Score: 5.5, OriginalScore: 5.5, Rank: 1,
		TenantID: "tenant-a", DocumentID: "doc", BlockID: "b1",
	}}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb, BM25: bm})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newExplainRouter(t, h, "tenant-a", admin.RoleAdmin)
	w := postExplain(r, retrieval.RetrieveRequest{Query: "hi", Explain: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) == 0 {
		t.Fatalf("expected at least one hit")
	}
	hit := got.Hits[0]
	if hit.Explain == nil {
		t.Fatalf("expected hit.explain populated")
	}
	if hit.Explain.RRFRank == 0 {
		t.Fatalf("expected RRFRank populated, got %+v", hit.Explain)
	}
	if hit.Explain.PolicyDecision != "allowed" {
		t.Fatalf("PolicyDecision: %q", hit.Explain.PolicyDecision)
	}
	// BM25 backend contributed → BM25Score populated from
	// Match.OriginalScore.
	hasBM25 := hit.Explain.BM25Score > 0
	hasVec := hit.Explain.VectorSimilarity > 0
	if !hasBM25 && !hasVec {
		t.Fatalf("expected at least one backend score populated, got %+v", hit.Explain)
	}
}

func TestExplain_OmittedForViewer(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "doc:b1", Score: 0.5, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newExplainRouter(t, h, "tenant-a", admin.RoleViewer)
	w := postExplain(r, retrieval.RetrieveRequest{Query: "hi", Explain: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) == 0 || got.Hits[0].Explain != nil {
		t.Fatalf("viewer should not see explain block: %+v", got.Hits)
	}
}

func TestExplain_OmittedForAnonymous(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "doc:b1", Score: 0.5, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newExplainRouter(t, h, "tenant-a", "")
	w := postExplain(r, retrieval.RetrieveRequest{Query: "hi", Explain: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) == 0 {
		t.Fatalf("expected hits")
	}
	if got.Hits[0].Explain != nil {
		t.Fatalf("anonymous should not see explain block")
	}
}

func TestExplain_FeatureFlagOverride(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "doc:b1", Score: 0.5, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:       vs,
		Embedder:          emb,
		ExplainEnvEnabled: true,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	// Anonymous caller — but feature flag is on.
	r := newExplainRouter(t, h, "tenant-a", "")
	w := postExplain(r, retrieval.RetrieveRequest{Query: "hi", Explain: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) == 0 || got.Hits[0].Explain == nil {
		t.Fatalf("flag-on should populate explain even for anonymous: %+v", got.Hits)
	}
}

// TestBuildExplain_MultiSourceDoesNotMisattributeBM25Score regression-tests
// the fix for BUG_pr-review-job-..._0002. When a match was contributed
// by multiple backends (e.g. vector + BM25), the prior implementation
// looped over m.Sources and assigned m.OriginalScore to every
// backend's score field. Since OriginalScore is BM25-specific
// (merger.go:63), the BM25 number leaked into VectorSimilarity and
// MemoryScore. The fix only projects OriginalScore onto BM25Score.
func TestBuildExplain_MultiSourceDoesNotMisattributeBM25Score(t *testing.T) {
	t.Parallel()
	// Match contributed by both vector and BM25 streams. The
	// merger preserved the BM25 stream's score in OriginalScore
	// (its only consumer) and zeroed Score with the RRF fused
	// score.
	m := &retrieval.Match{
		ID:            "doc:b1",
		Source:        retrieval.SourceVector,
		Sources:       []string{retrieval.SourceVector, retrieval.SourceBM25},
		Score:         0.0167, // post-merger RRF
		OriginalScore: 7.5,    // BM25 lexical relevance
		Rank:          1,
	}
	exp := retrieval.BuildExplain(m, 0.0167)
	if exp == nil {
		t.Fatal("expected non-nil explain")
	}
	if exp.BM25Score != 7.5 {
		t.Fatalf("BM25Score: got %v want 7.5", exp.BM25Score)
	}
	if exp.VectorSimilarity != 0 {
		t.Fatalf("VectorSimilarity must NOT be set from OriginalScore; got %v", exp.VectorSimilarity)
	}
	if exp.MemoryScore != 0 {
		t.Fatalf("MemoryScore must NOT be set from OriginalScore; got %v", exp.MemoryScore)
	}
}

// TestBuildExplain_VectorOnlyMatchHasNoMisattribution covers the
// edge case where a match is contributed by a non-BM25 backend
// only. Since the merger doesn't preserve OriginalScore for those
// backends, the corresponding score field must stay zero.
func TestBuildExplain_VectorOnlyMatchHasNoMisattribution(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{
		ID:            "doc:b1",
		Source:        retrieval.SourceVector,
		Sources:       []string{retrieval.SourceVector},
		Score:         0.91,
		OriginalScore: 0, // not preserved for vector
	}
	exp := retrieval.BuildExplain(m, 0.91)
	if exp.VectorSimilarity != 0 {
		t.Fatalf("VectorSimilarity should remain 0 (OriginalScore not vector-aware), got %v", exp.VectorSimilarity)
	}
	if exp.BM25Score != 0 {
		t.Fatalf("BM25Score should remain 0 for vector-only match, got %v", exp.BM25Score)
	}
}

func TestExplain_NotRequestedReturnsNil(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "doc:b1", Score: 0.5, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newExplainRouter(t, h, "tenant-a", admin.RoleAdmin)
	w := postExplain(r, retrieval.RetrieveRequest{Query: "hi"}) // explain not set
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Hits) == 0 {
		t.Fatalf("expected hits")
	}
	if got.Hits[0].Explain != nil {
		t.Fatalf("admin without explain:true should not see explain block")
	}
}

// TestExplain_BackendContributions_Round13 — Round-13 Task 7.
//
// With vector + BM25 backends both contributing and explain=true,
// the response top-level must include a backend_contributions map
// reporting how many top-K hits each backend contributed.
func TestExplain_BackendContributions_Round13(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "doc:b1", Score: 0.91, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
				"title": "T", "text": "x", "privacy_label": "internal",
			}},
			{ID: "doc:b2", Score: 0.80, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b2",
				"title": "T2", "text": "y", "privacy_label": "internal",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	bm := &stubBM25{matches: []*retrieval.Match{{
		ID: "doc:b1", Source: retrieval.SourceBM25, Score: 5.5, OriginalScore: 5.5, Rank: 1,
		TenantID: "tenant-a", DocumentID: "doc", BlockID: "b1",
	}}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb, BM25: bm})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newExplainRouter(t, h, "tenant-a", admin.RoleAdmin)
	w := postExplain(r, retrieval.RetrieveRequest{Query: "hi", Explain: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.BackendContributions == nil {
		t.Fatalf("expected backend_contributions to be populated; got nil")
	}
	// Vector contributed two hits (b1 and b2); BM25 contributed
	// one (b1 merged with the vector match). The exact counts
	// depend on Match.Sources after the merge.
	if got.BackendContributions[retrieval.SourceVector] == 0 {
		t.Fatalf("expected vector contribution > 0; got %+v", got.BackendContributions)
	}
	if got.BackendContributions[retrieval.SourceBM25] == 0 {
		t.Fatalf("expected bm25 contribution > 0; got %+v", got.BackendContributions)
	}
}

// TestExplain_BackendContributions_OmittedWhenNotExplain — Round-13 Task 7.
func TestExplain_BackendContributions_OmittedWhenNotExplain(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "doc:b1", Score: 0.91, Payload: map[string]any{
				"tenant_id": "tenant-a", "document_id": "doc", "block_id": "b1",
				"title": "T", "text": "x", "privacy_label": "internal",
			}},
		},
	}
	emb := &fakeEmbedder{vec: []float32{1, 2, 3}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: emb})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newExplainRouter(t, h, "tenant-a", admin.RoleAdmin)
	// explain=false (default)
	w := postExplain(r, retrieval.RetrieveRequest{Query: "hi"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.BackendContributions != nil {
		t.Fatalf("expected backend_contributions nil when explain=false; got %+v", got.BackendContributions)
	}
}
