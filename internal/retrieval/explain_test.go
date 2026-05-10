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
