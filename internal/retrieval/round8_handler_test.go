package retrieval_test

// round8_handler_test.go — Round-8 Tasks 9, 10, 15, 16.
//
// Exercises the new handler hooks (LatencyBudgetLookup,
// CacheTTLLookup, PinLookup, A/B router → QueryAnalytics) on the
// gin /v1/retrieve path.

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

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func newRound8Router(t *testing.T, h *retrieval.Handler) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "t-1")
		c.Next()
	})
	h.Register(&r.RouterGroup)
	return r
}

func round8DoRetrieve(t *testing.T, r *gin.Engine, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------- Task 16: pin lookup wired into the handler ----------

func TestHandler_PinLookup_PinsAppearInResponse(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "v1", Score: 0.9, Payload: map[string]any{"tenant_id": "t-1", "privacy_label": "public"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1}},
		DefaultTopK:        5,
		DefaultPrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	var lookups int32
	h.SetPinLookup(func(_ context.Context, tenantID, _ string) []retrieval.PinnedHit {
		atomic.AddInt32(&lookups, 1)
		if tenantID != "t-1" {
			t.Errorf("wrong tenant: %s", tenantID)
		}
		return []retrieval.PinnedHit{{ChunkID: "pinned-1", Position: 0}}
	})

	r := newRound8Router(t, h)
	w := round8DoRetrieve(t, r, map[string]any{"query": "hello"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&lookups) == 0 {
		t.Fatalf("PinLookup was not invoked")
	}
	var resp retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].ID != "pinned-1" {
		t.Fatalf("expected pinned-1 first; got %+v", resp.Hits)
	}
}

// ---------- Task 9: latency budget bounds the request ctx ----------

func TestHandler_LatencyBudgetLookup_Invoked(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "v1", Score: 0.9, Payload: map[string]any{"tenant_id": "t-1", "privacy_label": "public"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1}},
		DefaultTopK:        5,
		DefaultPrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	var called int32
	h.SetLatencyBudgetLookup(func(_ context.Context, tenantID string) (time.Duration, bool) {
		atomic.AddInt32(&called, 1)
		if tenantID != "t-1" {
			t.Errorf("wrong tenant: %s", tenantID)
		}
		return 500 * time.Millisecond, true
	})

	r := newRound8Router(t, h)
	w := round8DoRetrieve(t, r, map[string]any{"query": "hello"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&called) == 0 {
		t.Fatalf("LatencyBudgetLookup was not invoked")
	}
}

// ---------- Task 15: A/B route stamped on QueryAnalytics ----------

type round8FakeRouter struct{}

func (round8FakeRouter) Route(_ string, _, _ string) (*retrieval.ABTestRouteResult, error) {
	return &retrieval.ABTestRouteResult{ExperimentName: "exp-x", Arm: "variant"}, nil
}

func TestHandler_ABRouter_StampsQueryAnalytics(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "v1", Score: 0.9, Payload: map[string]any{"tenant_id": "t-1", "privacy_label": "public"}},
	}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           &fakeEmbedder{vec: []float32{1}},
		DefaultTopK:        5,
		DefaultPrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	h.SetABTestRouter(round8FakeRouter{})

	var captured retrieval.QueryAnalyticsEvent
	var mu sync.Mutex
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = evt
	})

	r := newRound8Router(t, h)
	w := round8DoRetrieve(t, r, map[string]any{"query": "hello", "experiment_name": "exp-x"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if captured.ExperimentName != "exp-x" || captured.ExperimentArm != "variant" {
		t.Fatalf("expected exp-x/variant; got %s/%s", captured.ExperimentName, captured.ExperimentArm)
	}
	if captured.QueryText != "hello" || captured.TenantID != "t-1" {
		t.Fatalf("event fields: %+v", captured)
	}
}
