//go:build e2e

// Phase 8 / Task 16 fault-injection / degradation e2e tests.
//
// docs/ARCHITECTURE.md §8 promises specific behaviours when individual
// retrieval backends are degraded:
//
//   - Vector down (Qdrant) → BM25 / Graph / Memory still serve a result
//     and the response carries policy.degraded["vector"].
//   - Cache down (Redis) → retrieval bypasses the cache and full fan-out
//     still succeeds.
//   - Slow backend (>per-backend deadline) → retrieval completes within
//     the budget using the remaining backends.
//   - All backends down → response carries policy.degraded for every
//     backend and the HTTP status is 200 (never 5xx).
//
// These tests exercise the in-process retrieval handler with stubbed
// backends so the assertions are deterministic. They live under the
// e2e build tag because they exercise full request paths and depend
// on external test scaffolding (gin, audit context).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

type degradeVS struct {
	hits      []storage.QdrantHit
	delay     time.Duration
	searchErr error
}

func (d *degradeVS) Search(ctx context.Context, _ string, _ []float32, _ storage.SearchOpts) ([]storage.QdrantHit, error) {
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if d.searchErr != nil {
		return nil, d.searchErr
	}
	return d.hits, nil
}

type fakeEmb struct{ vec []float32 }

func (f fakeEmb) EmbedQuery(_ context.Context, _ string, _ string) ([]float32, error) {
	return f.vec, nil
}

func newRetrievalRouter(t *testing.T, h *retrieval.Handler, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, tenantID); c.Next() })
	h.Register(&r.RouterGroup)
	return r
}

func TestDegrade_VectorDownDoesNotFail(t *testing.T) {
	requireE2E(t)
	t.Parallel()
	vs := &degradeVS{searchErr: errors.New("qdrant: connection refused")}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: fakeEmb{vec: []float32{1, 2, 3}}})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRetrievalRouter(t, h, "tenant-deg-1")
	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "hi"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even with vector down, got %d body=%s", w.Code, w.Body.String())
	}
	var resp retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Policy.Degraded) == 0 {
		t.Fatalf("expected degraded backend, got %+v", resp.Policy)
	}
}

func TestDegrade_AllBackendsDownReturnsEmpty(t *testing.T) {
	requireE2E(t)
	t.Parallel()
	vs := &degradeVS{searchErr: errors.New("qdrant down")}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{VectorStore: vs, Embedder: fakeEmb{vec: []float32{0}}})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRetrievalRouter(t, h, "tenant-deg-2")
	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "anything"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code/100 == 5 {
		t.Fatalf("expected non-5xx, got %d body=%s", w.Code, w.Body.String())
	}
	var resp retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hits) != 0 {
		t.Fatalf("expected zero hits, got %d", len(resp.Hits))
	}
}

func TestDegrade_SlowBackendStillCompletesWithinBudget(t *testing.T) {
	requireE2E(t)
	t.Parallel()
	vs := &degradeVS{delay: 800 * time.Millisecond}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    fakeEmb{vec: []float32{1}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRetrievalRouter(t, h, "tenant-deg-3")
	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "slow"})
	start := time.Now()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	dur := time.Since(start)
	// The handler defaults cap fan-out at <2s; the test asserts the
	// envelope is sent without the slow backend stalling the budget.
	if dur > 5*time.Second {
		t.Fatalf("slow backend held the request too long: %s", dur)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}
