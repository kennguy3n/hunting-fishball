//go:build e2e

// Package benchmark — Phase 3 Task 13: retrieval-only P95 latency
// budget enforcement.
//
// `p95_e2e_test.go` covers the round-trip budget (ingest →
// retrieve, Phase 1 exit criterion). This file enforces the
// stricter retrieval-only budget (Phase 3 exit criterion):
// 200 retrieval queries through the gin handler stack against
// synthetic vector + BM25 backends with 100 deterministic hits
// pre-seeded.
//
// The retrieval-only budget is < 500 ms P95. Failing this assertion
// indicates a regression in: errgroup fan-out, the merger, the
// reranker, or the policy filter — the four components on the
// hot path for every retrieval request.
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

// TestE2E_RetrievalP95Pure measures retrieval P95 against the
// stricter Phase 3 budget (500 ms). It complements TestE2E_RetrieveP95
// in `p95_e2e_test.go` which enforces the looser Phase 1 round-trip
// budget on the same pipeline.
//
// The number of indexed documents (100 synthetic hits across
// vector + BM25) and queries (200) come straight from the spec in
// `docs/PHASES.md` Phase 3 exit criteria.
func TestE2E_RetrievalP95Pure(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	const (
		indexedDocsN = 100
		queriesN     = 200
		retrievalP95 = 500 * time.Millisecond
	)

	// Seed both backends with 100 hits each so the merger has
	// real work to do (deduping, score normalisation).
	vs := &fakeVS{hits: synthVectorHits(indexedDocsN)}
	bm := &fakeBM25{hits: synthBM25Hits(indexedDocsN)}

	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmb{vec: []float32{0.1, 0.2, 0.3}},
		BM25:        bm,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "bench-tenant"); c.Next() })
	h.Register(&r.RouterGroup)

	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "context engine retrieval", PrivacyMode: "secret"})

	// 5-iteration warm-up so JIT / reflect caches don't skew the
	// first measurements.
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("warm-up status=%d body=%s", w.Code, w.Body.String())
		}
	}

	durations := make([]time.Duration, 0, queriesN)
	for i := 0; i < queriesN; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		t0 := time.Now()
		r.ServeHTTP(w, req)
		durations = append(durations, time.Since(t0))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[percentileIdx(queriesN, 50)]
	p95 := durations[percentileIdx(queriesN, 95)]
	p99 := durations[percentileIdx(queriesN, 99)]

	t.Logf("retrieval-only p50=%v p95=%v p99=%v over %d queries", p50, p95, p99, queriesN)
	if p95 > retrievalP95 {
		t.Fatalf("retrieval-only P95 %v exceeds Phase 3 budget %v", p95, retrievalP95)
	}

	// Print a JSON-friendly summary line for downstream
	// dashboards / artefact uploads.
	fmt.Printf("RETRIEVAL_ONLY_P95 p50=%dus p95=%dus p99=%dus n=%d\n",
		p50.Microseconds(), p95.Microseconds(), p99.Microseconds(), queriesN)

	// Reference imports to keep the linter quiet — we only need
	// these in the rare path where the test fails to compile.
	_ = context.Background
}
