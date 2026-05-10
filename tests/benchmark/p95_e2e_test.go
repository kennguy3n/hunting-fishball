//go:build e2e

// Package benchmark — Phase 1/3 Task 18: end-to-end P95 retrieval
// and round-trip latency budget enforcement.
//
// This file deliberately avoids docker-compose. It exercises the
// retrieval handler in-process against synthetic vector + BM25 +
// graph + memory backends so CI can enforce the latency budget on
// every PR. The "real" docker-compose round-trip lives next to the
// connector smoke test (`make test-connector-smoke`); the budget
// enforced here is for the handler glue, where a regression in
// fan-out scheduling, merger, reranker, or policy filter would
// blow past the SLO.
//
// Targets (ARCHITECTURE.md §5):
//   - retrieval P95 < 500 ms
//   - end-to-end round-trip P95 < 1 s
package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

// fakeGraph and fakeMemory complete the synthetic backend set so
// we exercise every fan-out branch.
type fakeGraph struct{ hits []*retrieval.Match }

func (f *fakeGraph) Traverse(_ context.Context, _, _ string, _ int) ([]*retrieval.Match, error) {
	return f.hits, nil
}

type fakeMemory struct{ hits []*retrieval.Match }

func (f *fakeMemory) Search(_ context.Context, _, _ string, _ int) ([]*retrieval.Match, error) {
	return f.hits, nil
}

// TestE2E_RetrieveP95 issues N retrieval requests through the full
// gin handler stack (cache → fan-out → merger → reranker → policy
// filter) and asserts the latency budget. The assertion is the
// reason this is a Test, not a Benchmark — `go test -bench` does
// not fail when budgets are exceeded.
func TestE2E_RetrieveP95(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	const (
		queriesN     = 200
		retrievalP95 = 500 * time.Millisecond
		roundTripP95 = 1 * time.Second
	)

	vs := &fakeVS{hits: synthVectorHits(20)}
	bm := &fakeBM25{hits: synthBM25Hits(20)}
	gr := &fakeGraph{hits: nil}
	mem := &fakeMemory{hits: nil}

	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmb{vec: []float32{0.1, 0.2, 0.3}},
		BM25:        bm,
		Graph:       gr,
		Memory:      mem,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "bench-tenant"); c.Next() })
	h.Register(&r.RouterGroup)

	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "context engine", PrivacyMode: "secret"})

	durations := make([]time.Duration, 0, queriesN)

	// Warm-up — JIT / cache the gin route table, etc.
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("warm-up status: %d body: %s", w.Code, w.Body.String())
		}
	}

	for i := 0; i < queriesN; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		t0 := time.Now()
		r.ServeHTTP(w, req)
		durations = append(durations, time.Since(t0))
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
		}
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[percentileIdx(queriesN, 50)]
	p95 := durations[percentileIdx(queriesN, 95)]
	p99 := durations[percentileIdx(queriesN, 99)]

	t.Logf("retrieval p50=%v p95=%v p99=%v over %d queries", p50, p95, p99, queriesN)

	if p95 > retrievalP95 {
		t.Fatalf("retrieval P95 %v exceeds budget %v", p95, retrievalP95)
	}
	// The "round-trip" budget is the same path here because there
	// is no separate ingest step — the synthetic backends embody
	// already-ingested data. The budget is still enforced so a
	// future change that adds work to the round-trip (e.g. a sync
	// fan-out write to mem0) is caught here.
	if p95 > roundTripP95 {
		t.Fatalf("round-trip P95 %v exceeds budget %v", p95, roundTripP95)
	}

	// Validate the new RetrieveResponse.Timings field is populated.
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// vector + bm25 ran (graph/memory fed empty hits but the
	// backends were wired and timed); so vector_ms and bm25_ms must
	// be non-negative — they're allowed to be 0 on extremely fast
	// machines but never negative.
	for name, v := range map[string]int64{
		"vector_ms": resp.Timings.VectorMs,
		"bm25_ms":   resp.Timings.BM25Ms,
		"merge_ms":  resp.Timings.MergeMs,
		"rerank_ms": resp.Timings.RerankMs,
	} {
		if v < 0 {
			t.Fatalf("timings.%s = %d (negative)", name, v)
		}
	}

	// Print as JSON-friendly summary line for downstream
	// dashboards / artefact uploads.
	fmt.Printf("RETRIEVE_P95 p50=%dus p95=%dus p99=%dus n=%d\n",
		p50.Microseconds(), p95.Microseconds(), p99.Microseconds(), queriesN)
}
