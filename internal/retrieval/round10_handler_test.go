package retrieval_test

// round10_handler_test.go — Round-10 Task 1.
//
// Strengthens the Round-8 latency-budget coverage: not only is
// the lookup invoked, the request's downstream context inherits
// the configured budget as a deadline. This is what proves the
// per-tenant budget actually shortens the fan-out — without the
// deadline check the lookup could fire and be ignored.
//
// The embedder is the cleanest seam to observe the per-request
// context: the handler invokes it with the (LatencyBudget-bound)
// request ctx BEFORE per-backend timeouts wrap the vector / bm25
// fan-out, so the deadline we capture there matches the budget
// directly.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// deadlineCapturingEmbedder satisfies retrieval.Embedder and
// records the deadline carried by the ctx passed into EmbedQuery.
type deadlineCapturingEmbedder struct {
	mu       sync.Mutex
	deadline time.Time
	hadDL    bool
	vec      []float32
}

func (e *deadlineCapturingEmbedder) EmbedQuery(ctx context.Context, _ string, _ string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		e.deadline = dl
		e.hadDL = true
	}
	return e.vec, nil
}

func TestHandler_LatencyBudgetLookup_BoundsRequestDeadline(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "v1", Score: 0.9, Payload: map[string]any{"tenant_id": "t-1", "privacy_label": "public"}},
	}}
	emb := &deadlineCapturingEmbedder{vec: []float32{1}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           emb,
		DefaultTopK:        5,
		DefaultPrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	const budget = 750 * time.Millisecond
	var called int32
	h.SetLatencyBudgetLookup(func(_ context.Context, tenantID string) (time.Duration, bool) {
		atomic.AddInt32(&called, 1)
		if tenantID != "t-1" {
			t.Errorf("wrong tenant: %s", tenantID)
		}
		return budget, true
	})

	r := newRound8Router(t, h)
	start := time.Now()
	w := round8DoRetrieve(t, r, map[string]any{"query": "hello"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&called) == 0 {
		t.Fatalf("LatencyBudgetLookup not invoked")
	}
	if !emb.hadDL {
		t.Fatalf("embedder ctx had no deadline — budget was not applied")
	}
	// The deadline should be roughly start+budget. We compare
	// against `start` to make the bound independent of how long
	// the handler took to reach the embedder call.
	dlOffset := emb.deadline.Sub(start)
	if dlOffset > budget+250*time.Millisecond {
		t.Fatalf("deadline too far in the future: offset=%v budget=%v", dlOffset, budget)
	}
	if dlOffset < budget-50*time.Millisecond {
		t.Fatalf("deadline too close: offset=%v budget=%v", dlOffset, budget)
	}
}

// TestHandler_LatencyBudgetLookup_NoOverrideWhenLookupReturnsFalse
// confirms the absence path: a lookup that returns ok=false leaves
// the request context untouched. Even if the ambient httptest
// stack carries a deadline of its own, the budget must not
// further shorten it.
func TestHandler_LatencyBudgetLookup_NoOverrideWhenLookupReturnsFalse(t *testing.T) {
	t.Parallel()
	vs := &fakeVectorStore{hits: []storage.QdrantHit{
		{ID: "v1", Score: 0.9, Payload: map[string]any{"tenant_id": "t-1", "privacy_label": "public"}},
	}}
	emb := &deadlineCapturingEmbedder{vec: []float32{1}}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        vs,
		Embedder:           emb,
		DefaultTopK:        5,
		DefaultPrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var called int32
	h.SetLatencyBudgetLookup(func(_ context.Context, _ string) (time.Duration, bool) {
		atomic.AddInt32(&called, 1)
		return 0, false
	})
	r := newRound8Router(t, h)
	w := round8DoRetrieve(t, r, map[string]any{"query": "hello"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&called) == 0 {
		t.Fatalf("LatencyBudgetLookup not invoked")
	}
	// httptest does not impose a deadline so the embedder ctx
	// should have none either: the absence is the assertion.
	if emb.hadDL {
		t.Fatalf("embedder ctx unexpectedly had a deadline: %v", emb.deadline)
	}
}
