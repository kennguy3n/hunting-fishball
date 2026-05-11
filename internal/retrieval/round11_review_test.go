// round11_review_test.go — regression tests for the Devin Review
// findings on PR #20. The first wave (commit 5447dc7) covered:
//
//  1. The batch handler's `runOne` recorded query analytics with
//     `topK = len(resp.Hits)` when the sub-request omitted `top_k`,
//     instead of the effective `DefaultTopK` that
//     `RetrieveWithSnapshotCached.resolveTopKAndPrivacy` actually used
//     to run the retrieve. Result: the `query_analytics.top_k`
//     column under-reported batch traffic whenever a slot returned
//     fewer hits than the configured default.
//  2. `CacheWarmer.Warm` constructed `RetrieveRequest` without
//     `Source`, and `RetrieveWithSnapshotCached` did not call the
//     analytics recorder at all — so the `cache_warm` source value
//     (Round-11 Task 9) was dead code and warm-up traffic was
//     invisible in `query_analytics`.
//
// Both fixes consolidate analytics recording inside
// `RetrieveWithSnapshotCached`, so it now fires for both the batch
// handler AND the cache warmer with the correct `Source` tag.
//
// The second wave covers two follow-on findings Devin Review
// raised on the fix commit itself:
//
//  3. `stream_handler.go` emits the per-event explain trace
//     (per-backend timings + score breakdown) whenever
//     `req.Explain == true`, but the single-request handler
//     (`handler.go:611`) and batch handler (`batch_handler.go:86`)
//     both gate explain on `IsExplainAuthorized` first. Without
//     that gate any authenticated caller could request the SSE
//     stream's explain payload — a leak of scoring internals the
//     other two surfaces require admin role or the
//     `CONTEXT_ENGINE_EXPLAIN_ENABLED` flag to surface.
//  4. The cache-aware entry point passed `nil` for `backendTimings`
//     to `recordQueryAnalytics` on both cache-hit and cache-miss
//     paths. The data was available in `resp.Timings` (a
//     `RetrieveTimings` struct), and the gin handler at line 660
//     passes `backendTimingsMillis(backendTimings)`. Result: batch
//     + cache-warm analytics rows had empty `backend_timings`, so
//     per-backend latency dashboards only saw single-request
//     traffic.
package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// TestBatch_AnalyticsTopKReportsDefaultWhenOmitted asserts that
// when a batch sub-request omits `top_k`, the analytics row records
// the effective `DefaultTopK` rather than `len(resp.Hits)`.
//
// Pre-fix: the recorder saw TopK=1 (== len(resp.Hits)) for a
// sub-request that omitted top_k against a corpus with one match.
// Post-fix: the recorder sees TopK=10 (== DefaultTopK).
func TestBatch_AnalyticsTopKReportsDefaultWhenOmitted(t *testing.T) {
	t.Parallel()

	vs := &slowVectorStore{
		hits: []storage.QdrantHit{
			{ID: "only-one", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	r := newRouter(t, h, "tenant-a")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		// Critical: TopK is intentionally omitted (0). The
		// pre-fix bug recorded `len(resp.Hits)` (== 1 with this
		// fake), not the resolved DefaultTopK (== 10).
		{Query: "batch-q1"},
	}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 analytics event, got %d", len(captured))
	}
	evt := captured[0]
	if evt.Source != retrieval.QueryAnalyticsSourceBatch {
		t.Fatalf("expected source=%q, got %q", retrieval.QueryAnalyticsSourceBatch, evt.Source)
	}
	// HitCount confirms the fake produced one result.
	if evt.HitCount != 1 {
		t.Fatalf("expected HitCount=1, got %d", evt.HitCount)
	}
	// TopK MUST be DefaultTopK (10), NOT len(resp.Hits) (1).
	// Pre-fix this assertion failed with `TopK=1`, which is the
	// regression Devin Review flagged on batch_handler.go:162-164.
	if evt.TopK != 10 {
		t.Fatalf("expected analytics TopK=10 (== DefaultTopK), got %d "+
			"(regression: batch handler was reporting len(resp.Hits) "+
			"instead of the effective DefaultTopK when the sub-request "+
			"omitted top_k)", evt.TopK)
	}
}

// TestBatch_AnalyticsTopKRespectsExplicitValue locks down the
// happy-path where an explicit sub-request top_k is honoured by
// the analytics recorder. Without this guard a future refactor
// might over-correct the previous bug and clobber explicit
// values.
func TestBatch_AnalyticsTopKRespectsExplicitValue(t *testing.T) {
	t.Parallel()

	vs := &slowVectorStore{
		hits: []storage.QdrantHit{
			{ID: "only-one", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	r := newRouter(t, h, "tenant-a")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "batch-q1", TopK: 25},
	}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 analytics event, got %d", len(captured))
	}
	if got := captured[0].TopK; got != 25 {
		t.Fatalf("expected analytics TopK=25 (explicit value), got %d", got)
	}
}

// TestCacheWarmer_AnalyticsTaggedCacheWarmSource asserts that
// CacheWarmer.Warm causes the analytics recorder to fire with
// `source = "cache_warm"`. Pre-fix the `QueryAnalyticsSourceCacheWarm`
// constant existed but was dead code: the warmer's constructed
// request had `Source == ""` (defaulted to "user" downstream) and
// `RetrieveWithSnapshotCached` did not call the recorder at all.
//
// Post-fix the warmer sets `Source = QueryAnalyticsSourceCacheWarm`
// and `RetrieveWithSnapshotCached` records analytics for every
// caller, so warm-up traffic is now visible in `query_analytics`
// and distinguishable from organic `user` and `batch` rows.
func TestCacheWarmer_AnalyticsTaggedCacheWarmSource(t *testing.T) {
	t.Parallel()

	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-a", "title": "v"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	resolver := stubResolver{snap: retrieval.PolicySnapshot{}}
	w, err := retrieval.NewCacheWarmer(h, resolver)
	if err != nil {
		t.Fatalf("NewCacheWarmer: %v", err)
	}

	summary := w.Warm(context.Background(), []retrieval.WarmTuple{
		{TenantID: "tenant-a", Query: "popular-query-1"},
		{TenantID: "tenant-b", Query: "popular-query-2"},
	})
	if summary.Total != 2 || summary.Succeeded != 2 || summary.Failed != 0 {
		t.Fatalf("expected 2 succeeded / 0 failed; got %+v", summary)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("expected 2 analytics events (one per warmed tuple), got %d "+
			"(regression: CacheWarmer.Warm was not reaching the analytics "+
			"recorder at all, so the cache_warm source value was dead code)",
			len(captured))
	}
	for i, evt := range captured {
		if evt.Source != retrieval.QueryAnalyticsSourceCacheWarm {
			t.Fatalf("event %d: expected source=%q, got %q (regression: "+
				"warmer was not tagging analytics so all warm-up traffic "+
				"defaulted to source=\"user\")",
				i, retrieval.QueryAnalyticsSourceCacheWarm, evt.Source)
		}
	}

	// Spot-check tenant attribution — without this, a future
	// refactor could collapse the per-tuple source onto the wrong
	// tenant_id.
	tenants := map[string]bool{}
	for _, evt := range captured {
		tenants[evt.TenantID] = true
	}
	if !tenants["tenant-a"] || !tenants["tenant-b"] {
		t.Fatalf("expected analytics events for both tenant-a and tenant-b, got %v", tenants)
	}
}

// streamRouterForExplain wires the stream handler under a router
// where the auth middleware sets a tenant id and (optionally) a
// role. The role is what IsExplainAuthorized consults — with no
// role and no env opt-in, explain MUST be stripped.
func streamRouterForExplain(t *testing.T, h *retrieval.StreamHandler, tenantID, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		if role != "" {
			c.Set(admin.RoleContextKey, role)
		}
		c.Next()
	})
	h.Register(&r.RouterGroup)
	return r
}

// fixedExplainBackend is a one-shot stub backend so the stream
// handler's fan-out has a known set of matches to project into
// the explain payload.
type fixedExplainBackend struct {
	name    string
	matches []*retrieval.Match
}

func (s *fixedExplainBackend) Name() string { return s.name }
func (s *fixedExplainBackend) Search(context.Context, retrieval.RetrieveRequest) ([]*retrieval.Match, error) {
	return s.matches, nil
}

// concatMerger collapses the per-backend match map into one
// slice. The merger contract is opaque to these tests — they
// only inspect the SSE backend events, not the done payload's
// merged hits — so a passthrough is fine.
type concatMerger struct{}

func (concatMerger) Merge(_ context.Context, _ string, _ retrieval.RetrieveRequest, per map[string][]*retrieval.Match) ([]*retrieval.Match, error) {
	out := make([]*retrieval.Match, 0)
	for _, m := range per {
		out = append(out, m...)
	}
	return out, nil
}

// streamHasBackendExplain scans an SSE body for `event: backend`
// frames and reports whether ANY of them carried a non-empty
// `explain` block. Used both in the leak-prevention check and
// the admin-allow check.
func streamHasBackendExplain(t *testing.T, body string) bool {
	t.Helper()
	var sawExplain bool
	var event string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") || event != "backend" {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var ev retrieval.StreamBackendEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("decode backend SSE frame: %v data=%q", err, data)
		}
		if ev.Explain != nil {
			sawExplain = true
		}
	}
	return sawExplain
}

// streamHasDoneExplain returns whether the terminal `event: done`
// frame carried an aggregate explain block. The aggregate
// duplicates the per-backend timing map, so an unauthorised
// caller surfacing it is the same leak as the per-event trace.
func streamHasDoneExplain(t *testing.T, body string) bool {
	t.Helper()
	var sawExplain bool
	var event string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") || event != "done" {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var ev retrieval.StreamDoneEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("decode done SSE frame: %v data=%q", err, data)
		}
		if ev.Explain != nil {
			sawExplain = true
		}
	}
	return sawExplain
}

// TestStream_ExplainRequiresAuthorization is the pin-test for the
// Devin Review finding on stream_handler.go:180. A caller WITHOUT
// admin role and without the env opt-in must NOT receive the
// per-event explain trace or the aggregate done explain — even
// when they set `explain: true` on the request.
//
// Pre-fix: any authenticated caller could surface the explain
// payload by toggling the request flag.
// Post-fix: the handler strips `req.Explain` when
// IsExplainAuthorized returns false, mirroring the gate the
// single-request handler (handler.go:611) and batch handler
// (batch_handler.go:86) already applied.
func TestStream_ExplainRequiresAuthorization(t *testing.T) {
	t.Parallel()
	be := &fixedExplainBackend{
		name:    "vector",
		matches: []*retrieval.Match{{ID: "c1", Score: 0.7}},
	}
	h, err := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{
		Backends: []retrieval.StreamBackend{be},
		Merger:   concatMerger{},
		// ExplainEnvEnabled intentionally false — without this and
		// without an admin role the handler must strip explain.
	})
	if err != nil {
		t.Fatalf("NewStreamHandler: %v", err)
	}
	// No role on the context; auth middleware just sets the tenant.
	r := streamRouterForExplain(t, h, "tenant-noauth", "")

	body := strings.NewReader(`{"query":"hi","explain":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve/stream", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	if streamHasBackendExplain(t, w.Body.String()) {
		t.Fatalf("regression: unauthorised caller received per-event explain " +
			"trace on /v1/retrieve/stream — the handler must strip explain " +
			"unless IsExplainAuthorized returns true (matching the gate at " +
			"handler.go:611 + batch_handler.go:86)")
	}
	if streamHasDoneExplain(t, w.Body.String()) {
		t.Fatalf("regression: unauthorised caller received the aggregate " +
			"done explain block on /v1/retrieve/stream — same leak as the " +
			"per-event payload")
	}
}

// TestStream_ExplainEnabledByEnvFlag locks down the env-flag
// branch of IsExplainAuthorized for the stream handler. When
// ExplainEnvEnabled is true on the config (the deployment opted
// in via CONTEXT_ENGINE_EXPLAIN_ENABLED) every caller — admin or
// not — receives the explain payload.
func TestStream_ExplainEnabledByEnvFlag(t *testing.T) {
	t.Parallel()
	be := &fixedExplainBackend{
		name:    "vector",
		matches: []*retrieval.Match{{ID: "c1", Score: 0.7}},
	}
	h, err := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{
		Backends:          []retrieval.StreamBackend{be},
		Merger:            concatMerger{},
		ExplainEnvEnabled: true,
	})
	if err != nil {
		t.Fatalf("NewStreamHandler: %v", err)
	}
	r := streamRouterForExplain(t, h, "tenant-envflag", "")

	body := strings.NewReader(`{"query":"hi","explain":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve/stream", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	if !streamHasBackendExplain(t, w.Body.String()) {
		t.Fatalf("expected per-event explain trace when ExplainEnvEnabled=true")
	}
	if !streamHasDoneExplain(t, w.Body.String()) {
		t.Fatalf("expected aggregate done explain when ExplainEnvEnabled=true")
	}
}

// TestStream_ExplainEnabledForAdminRole locks down the RBAC
// branch of IsExplainAuthorized for the stream handler. When the
// caller's role is admin (or super_admin), explain flows even
// when the env flag is off.
func TestStream_ExplainEnabledForAdminRole(t *testing.T) {
	t.Parallel()
	be := &fixedExplainBackend{
		name:    "vector",
		matches: []*retrieval.Match{{ID: "c1", Score: 0.7}},
	}
	h, err := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{
		Backends: []retrieval.StreamBackend{be},
		Merger:   concatMerger{},
		// Env flag off — auth must come from the role on context.
	})
	if err != nil {
		t.Fatalf("NewStreamHandler: %v", err)
	}
	r := streamRouterForExplain(t, h, "tenant-admin", string(admin.RoleAdmin))

	body := strings.NewReader(`{"query":"hi","explain":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve/stream", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	if !streamHasBackendExplain(t, w.Body.String()) {
		t.Fatalf("expected per-event explain trace for admin role")
	}
}

// TestRetrieveWithSnapshotCached_RecordsBackendTimingsOnMiss is
// the pin-test for the Devin Review finding on handler.go:852
// (cache-miss path). RetrieveWithSnapshotCached must record the
// resolved per-backend timings into the analytics event rather
// than passing nil. Pre-fix the recorder saw an empty
// BackendTimings map on every batch + cache-warm row.
func TestRetrieveWithSnapshotCached_RecordsBackendTimingsOnMiss(t *testing.T) {
	t.Parallel()
	vs := &slowVectorStore{
		hits: []storage.QdrantHit{
			{ID: "miss-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-miss"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	// Route through the batch handler so the call lands on
	// RetrieveWithSnapshotCached, the centralised analytics path.
	r := newRouter(t, h, "tenant-miss")
	body, _ := json.Marshal(retrieval.BatchRequest{Requests: []retrieval.RetrieveRequest{
		{Query: "miss-q"},
	}})
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 analytics event, got %d", len(captured))
	}
	evt := captured[0]
	if evt.CacheHit {
		t.Fatalf("expected cache_hit=false (this is the miss-path test); got %+v", evt)
	}
	if evt.BackendTimings == nil {
		t.Fatalf("regression: BackendTimings is nil on the cache-miss path. " +
			"RetrieveWithSnapshotCached used to pass nil instead of projecting " +
			"resp.Timings; this dropped per-backend latency for every batch + " +
			"cache-warm row in query_analytics.")
	}
	// The map must carry the schema keys timingsToMap emits, so
	// dashboards can JOIN cache_hit + non-cache_hit rows on the
	// same column names without coalescing.
	for _, key := range []string{"vector_ms", "bm25_ms", "graph_ms", "memory_ms", "merge_ms", "rerank_ms"} {
		if _, ok := evt.BackendTimings[key]; !ok {
			t.Errorf("BackendTimings missing %q key; got keys = %v", key, mapKeys(evt.BackendTimings))
		}
	}
}

// TestRetrieveWithSnapshotCached_RecordsBackendTimingsOnHit is
// the cache-hit twin of the miss-path test. Even on cache-hit
// the recorder must see a populated (zero-valued is fine) map so
// downstream consumers see a consistent schema across both
// branches.
func TestRetrieveWithSnapshotCached_RecordsBackendTimingsOnHit(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{}
	vs := &fakeVectorStore{
		hits: []storage.QdrantHit{
			{ID: "hit-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-hit"}},
		},
	}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmbedder{vec: []float32{1, 2}},
		Cache:       cache,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	resolver := stubResolver{snap: retrieval.PolicySnapshot{}}
	warmer, err := retrieval.NewCacheWarmer(h, resolver)
	if err != nil {
		t.Fatalf("NewCacheWarmer: %v", err)
	}

	// Warm so the cache.Set bookend fires, then mirror Set→Get to
	// simulate a populated Redis (the in-test fakeCache doesn't
	// auto-mirror; see cache_warmer_test.go for the same pattern).
	_ = warmer.Warm(context.Background(), []retrieval.WarmTuple{
		{TenantID: "tenant-hit", Query: "popular"},
	})
	cache.mu.Lock()
	cache.getValue = cache.lastSet
	cache.mu.Unlock()

	// Reset the recorder so we only capture the cache-HIT event,
	// not the warm-up that populated the cache.
	var (
		mu       sync.Mutex
		captured []retrieval.QueryAnalyticsEvent
	)
	h.SetQueryAnalyticsRecorder(func(_ context.Context, evt retrieval.QueryAnalyticsEvent) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, evt)
	})

	// Second warm against the same tuple → serves from cache.
	summary := warmer.Warm(context.Background(), []retrieval.WarmTuple{
		{TenantID: "tenant-hit", Query: "popular"},
	})
	if summary.Total != 1 || summary.Succeeded != 1 {
		t.Fatalf("expected 1 succeeded on the cache-hit warm; got %+v", summary)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 analytics event for cache-hit warm; got %d", len(captured))
	}
	evt := captured[0]
	if !evt.CacheHit {
		t.Fatalf("expected cache_hit=true on the second warm; got %+v", evt)
	}
	if evt.BackendTimings == nil {
		t.Fatalf("regression: BackendTimings is nil on the cache-hit path. " +
			"Even though cache-hit timings are zero, the recorder must see a " +
			"populated map so downstream consumers can join cache_hit + " +
			"non-cache_hit rows on a consistent schema.")
	}
	for _, key := range []string{"vector_ms", "bm25_ms", "graph_ms", "memory_ms", "merge_ms", "rerank_ms"} {
		if _, ok := evt.BackendTimings[key]; !ok {
			t.Errorf("BackendTimings missing %q key on cache-hit path; got %v", key, mapKeys(evt.BackendTimings))
		}
	}
}

func mapKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
