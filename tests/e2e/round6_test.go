//go:build e2e

// Package e2e — round6_test.go — Round-6 Task 20.
//
// Exercises the key features from Round 6 end-to-end against the
// in-process implementations of the new packages. Unlike the
// Round-5 tests this file deliberately avoids any docker-compose
// dependencies so it can run on developer laptops and in CI even
// when Postgres/Qdrant/Kafka are unavailable.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

func adminRouter6(tenantID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, tenantID) })
	return r
}

func TestRound6_MMRDiversity(t *testing.T) {
	// Lambda=1.0 ⇒ pure relevance (passthrough); lambda=0.0 ⇒ max
	// diversity. The diversifier surface lives in
	// `internal/retrieval` and is exercised by
	// diversifier_test.go directly; here we only smoke-test the
	// public ctor compiles + does not panic on a trivial input.
	sim := func(a, b *retrieval.Match) float32 {
		if a.ID == b.ID {
			return 1
		}
		return 0
	}
	d := retrieval.NewMMRDiversifier(sim)
	in := []*retrieval.Match{{ID: "a", Score: 1}, {ID: "b", Score: 0.5}}
	out := d.Diversify(context.Background(), in, 0.5, 2)
	if len(out) != 2 {
		t.Fatalf("expected 2; got %d", len(out))
	}
}

func TestRound6_SemanticDedup(t *testing.T) {
	d := pipeline.NewDeduplicator(pipeline.DedupConfig{Enabled: true, Threshold: 0.95})
	doc := &pipeline.Document{TenantID: "t", DocumentID: "d"}
	blocks := []pipeline.Block{
		{BlockID: "1", Text: "alpha"},
		{BlockID: "2", Text: "alpha-near-duplicate"},
		{BlockID: "3", Text: "completely different"},
	}
	embeddings := [][]float32{
		{1, 0},
		{0.999, 0.001},
		{0, 1},
	}
	res, err := d.Apply(context.Background(), doc, blocks, embeddings)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Blocks) != 2 || res.Removed != 1 {
		t.Fatalf("expected 2 kept / 1 removed; got kept=%d removed=%d", len(res.Blocks), res.Removed)
	}
}

func TestRound6_QueryExpansion(t *testing.T) {
	store := retrieval.NewInMemorySynonymStore()
	_ = store.Set(context.Background(), "tenant-1", map[string][]string{
		"vpn": {"virtual private network", "tunnel"},
	})
	exp := retrieval.NewSynonymExpander(store, 2)
	out, err := exp.Expand(context.Background(), "tenant-1", "vpn outage")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(out, "tunnel") && !strings.Contains(out, "virtual private network") {
		t.Fatalf("expansion missing synonyms; got %q", out)
	}
}

func TestRound6_ChunkACL(t *testing.T) {
	acl := policy.NewChunkACL([]policy.ChunkACLTag{{
		ChunkID: "chunk-1", Decision: policy.ChunkACLDecisionDeny,
	}})
	if acl.Len() != 1 {
		t.Fatalf("len=%d", acl.Len())
	}
	if acl.Evaluate(policy.ChunkACLAttrs{ChunkID: "chunk-1"}) != policy.ChunkACLDecisionDeny {
		t.Fatalf("expected deny for chunk-1")
	}
	if acl.Evaluate(policy.ChunkACLAttrs{ChunkID: "chunk-2"}) == policy.ChunkACLDecisionDeny {
		t.Fatalf("chunk-2 must not be denied")
	}
}

type fakeBucket struct{ rate float64 }

func (b *fakeBucket) Wait(_ context.Context, _ string) error { return nil }
func (b *fakeBucket) SetRate(_ context.Context, _ string, r float64) error {
	b.rate = r
	return nil
}

func TestRound6_AdaptiveRate429(t *testing.T) {
	bucket := &fakeBucket{}
	lim, err := connector.NewAdaptiveRateLimiter(connector.AdaptiveRateLimiterConfig{
		BaseRate: 100, Bucket: bucket,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if err := lim.Throttled(context.Background(), "k"); err != nil {
		t.Fatalf("throttled: %v", err)
	}
	if r := lim.CurrentRate(); r >= 100 {
		t.Fatalf("rate should drop after Throttled; got %f", r)
	}
}

func TestRound6_BackpressureMetricEmission(t *testing.T) {
	observability.ResetForTest()
	observability.PipelineChannelDepth.WithLabelValues("fetch").Set(7)
	// Smoke: no panic and the metric is registered. Asserting on a
	// running value requires the prometheus testutil package which is
	// already covered by internal/pipeline/backpressure_test.go.
}

func TestRound6_NotificationDispatchSmoke(t *testing.T) {
	store := admin.NewInMemoryNotificationStore()
	_ = store.Create(&admin.NotificationPreference{
		TenantID:  "t",
		EventType: "source.synced",
		Channel:   admin.NotificationChannelWebhook,
		Target:    "https://example.invalid/hook",
		Enabled:   true,
	})
	disp, err := admin.NewNotificationDispatcher(store, &fakeDispatchDelivery{})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if err := disp.Dispatch(context.Background(), "t", "source.synced", map[string]any{"ok": true}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

type fakeDispatchDelivery struct{ called int }

func (f *fakeDispatchDelivery) Send(_ context.Context, _ string, _ admin.NotificationChannel, _ []byte) error {
	f.called++
	return nil
}

func TestRound6_APIVersionHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.APIVersionMiddleware(observability.APIVersionConfig{Supported: []string{"v1"}}))
	r.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Header().Get("X-API-Version") != "v1" {
		t.Fatalf("missing X-API-Version header")
	}
}

func TestRound6_PipelineDryRun(t *testing.T) {
	enum := &fakeReindexEnum{docs: []string{"a", "b"}}
	emit := &fakeReindexEmitter{}
	orch, err := pipeline.NewReindexOrchestrator(enum, emit)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := orch.Reindex(context.Background(), pipeline.ReindexRequest{
		TenantID: "t", SourceID: "s", DryRun: true,
	})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if res.EventsEmitted != 0 {
		t.Fatalf("dry-run must emit nothing; got %d", res.EventsEmitted)
	}
	if res.DocumentsEnumerated != 2 {
		t.Fatalf("expected 2 enumerated; got %d", res.DocumentsEnumerated)
	}
}

type fakeReindexEnum struct{ docs []string }

func (f *fakeReindexEnum) ListDocuments(_ context.Context, _ pipeline.ReindexRequest) ([]string, error) {
	return f.docs, nil
}

type fakeReindexEmitter struct{ count int }

func (f *fakeReindexEmitter) EmitEvent(_ context.Context, _ pipeline.IngestEvent) error {
	f.count++
	return nil
}

func TestRound6_StreamingSSE(t *testing.T) {
	be := &fakeStreamBackend{name: "v", matches: []*retrieval.Match{{ID: "c1"}}}
	h, err := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{
		Backends: []retrieval.StreamBackend{be}, Merger: fakeStreamMerger{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := adminRouter6("tenant-1")
	h.Register(r.Group("/"))
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve/stream", strings.NewReader(`{"query":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "event: backend") || !strings.Contains(rr.Body.String(), "event: done") {
		t.Fatalf("missing SSE markers in body=%s", rr.Body)
	}
}

type fakeStreamBackend struct {
	name    string
	matches []*retrieval.Match
}

func (f *fakeStreamBackend) Name() string { return f.name }
func (f *fakeStreamBackend) Search(_ context.Context, _ retrieval.RetrieveRequest) ([]*retrieval.Match, error) {
	return f.matches, nil
}

type fakeStreamMerger struct{}

func (fakeStreamMerger) Merge(_ context.Context, _ string, _ retrieval.RetrieveRequest, per map[string][]*retrieval.Match) ([]*retrieval.Match, error) {
	var out []*retrieval.Match
	for _, m := range per {
		out = append(out, m...)
	}
	return out, nil
}

func TestRound6_ShardScheduler(t *testing.T) {
	now := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	sched, err := shard.NewScheduler(shard.SchedulerConfig{
		TenantLister:    &fakeLister{tenants: []string{"t1"}},
		Manifests:       &fakeManifests{},
		Submitter:       &fakeSubmitter{},
		FreshnessWindow: time.Hour,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if err := sched.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sched.Stats().Submitted != 1 {
		t.Fatalf("expected 1 submission; got %+v", sched.Stats())
	}
}

type fakeLister struct{ tenants []string }

func (f *fakeLister) ActiveTenants(_ context.Context) ([]string, error) { return f.tenants, nil }

type fakeManifests struct{}

func (f *fakeManifests) LatestByTenant(_ context.Context, _ string) ([]*shard.ManifestSummary, error) {
	return nil, nil
}

type fakeSubmitter struct{ count int }

func (f *fakeSubmitter) SubmitShardRequest(_ context.Context, _, _ string) error {
	f.count++
	return nil
}

func TestRound6_RetryAnalyticsSnapshot(t *testing.T) {
	observability.ResetForTest()
	a := pipeline.NewRetryAnalytics()
	_ = a.RecordAttempt("fetch", "k", pipeline.RetryOutcomeRetry, "timeout")
	_ = a.RecordAttempt("fetch", "k", pipeline.RetryOutcomeSuccess, "")
	snap := a.Snapshot()
	if len(snap) != 1 || snap[0].SuccessAfterRtry != 1 {
		t.Fatalf("unexpected: %+v", snap)
	}
}

func TestRound6_TenantExportLifecycle(t *testing.T) {
	store := admin.NewInMemoryTenantExportJobStore()
	h, err := admin.NewTenantExportHandler(admin.TenantExportConfig{
		JobStore: store, Collector: &stubExportCol{}, Publisher: &stubExportPub{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := adminRouter6("tenant-1")
	h.Register(r.Group("/"))
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/tenant-1/export", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("post status=%d", rr.Code)
	}
	var job admin.TenantExportJob
	_ = json.Unmarshal(rr.Body.Bytes(), &job)
	if err := h.RunJobSync(context.Background(), job.ID, "tenant-1"); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := store.Get(context.Background(), "tenant-1", job.ID)
	if got.Status != admin.TenantExportStatusCompleted {
		t.Fatalf("status=%s", got.Status)
	}
}

type stubExportCol struct{}

func (stubExportCol) Collect(_ context.Context, tenantID string) (*admin.TenantExportPayload, error) {
	raw, _ := json.Marshal(map[string]any{"id": "s1"})
	return &admin.TenantExportPayload{TenantID: tenantID, Sources: []json.RawMessage{raw}}, nil
}

type stubExportPub struct{}

func (stubExportPub) Publish(_ context.Context, jobID string, _ []byte) (string, error) {
	return "blob://" + jobID, nil
}

func TestRound6_ConnectorTemplateApply(t *testing.T) {
	tmpl := &admin.ConnectorTemplate{DefaultConfig: admin.JSONMap{"a": 1, "b": 2}}
	merged := admin.ApplyTemplate(tmpl, admin.JSONMap{"b": 99})
	if merged["a"].(int) != 1 || merged["b"].(int) != 99 {
		t.Fatalf("merge wrong: %+v", merged)
	}
}

func TestRound6_IsolationAuditPasses(t *testing.T) {
	a, err := admin.NewIsolationAuditor([]admin.IsolationChecker{
		admin.CheckerFunc{BackendName: "qdrant", Fn: func(_ context.Context) admin.IsolationCheckResult {
			return admin.IsolationCheckResult{Pass: true, Inspected: 1}
		}},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	rep := a.Run(context.Background())
	if !rep.OverallPass || len(rep.Results) != 1 {
		t.Fatalf("unexpected: %+v", rep)
	}
}

func TestRound6_ABTestRoutingDeterministic(t *testing.T) {
	store := admin.NewInMemoryABTestStore()
	cfg := admin.ABTestConfig{
		TenantID: "t", ExperimentName: "x", Status: admin.ABTestStatusActive,
		TrafficSplitPercent: 50,
		ControlConfig:       map[string]any{"k": "control"},
		VariantConfig:       map[string]any{"k": "variant"},
	}
	if err := store.Upsert(&cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	router := admin.NewABTestRouter(store)
	r1, err := router.Route("t", "x", "user-1")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	r2, _ := router.Route("t", "x", "user-1")
	if r1 == nil || r2 == nil || r1.Arm != r2.Arm {
		t.Fatalf("routing must be deterministic for the same key")
	}
}

// helpers

func ids(in []*retrieval.Match) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.ID
	}
	return out
}

func indexOf(in []*retrieval.Match, id string) int {
	for i, m := range in {
		if m.ID == id {
			return i
		}
	}
	return -1
}
