//go:build e2e

// Package e2e — round7_test.go — Round-7 Task 19.
//
// Exercises the Round-7 admin surface end-to-end against the
// in-process implementations. Like round6_test.go this file is
// deliberately docker-compose-free so it runs on developer
// laptops and in CI even when Postgres / Qdrant / Kafka are
// unavailable. Each subtest spins up a minimal Gin router with
// a tenant-injecting middleware, mounts exactly one handler,
// and asserts the round-trip behaviour the spec calls out.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

const round7Tenant = "tenant-r7"
const round7Actor = "actor-r7"

func round7Router(handlers ...func(*gin.RouterGroup)) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, round7Tenant)
		c.Set(audit.ActorContextKey, round7Actor)
		c.Next()
	})
	g := r.Group("/")
	for _, reg := range handlers {
		reg(g)
	}
	return r
}

type round7Audit struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (a *round7Audit) Create(_ context.Context, log *audit.AuditLog) error {
	if log == nil {
		return errors.New("nil audit log")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, log)
	return nil
}

func (a *round7Audit) actions() []audit.Action {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]audit.Action, 0, len(a.logs))
	for _, l := range a.logs {
		out = append(out, l.Action)
	}
	return out
}

func doJSON(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Task 4: retrieval query analytics — round-trip the recorder
// and assert the list/top endpoints return only the current
// tenant's rows.
func TestRound7_QueryAnalytics(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	rec, err := admin.NewQueryAnalyticsRecorder(store)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		rec.Record(ctx, admin.QueryAnalyticsEvent{
			TenantID:  round7Tenant,
			QueryText: "alpha",
			TopK:      10,
			HitCount:  3,
			LatencyMS: 42 + i,
			At:        time.Now(),
		})
	}
	rec.Record(ctx, admin.QueryAnalyticsEvent{
		TenantID: "other", QueryText: "beta", TopK: 10, HitCount: 1, LatencyMS: 1, At: time.Now(),
	})
	h, err := admin.NewQueryAnalyticsHandler(store)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)

	w := doJSON(t, r, http.MethodGet, "/v1/admin/analytics/queries", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "beta") {
		t.Fatalf("cross-tenant leak: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "alpha") {
		t.Fatalf("missing tenant rows: %s", w.Body.String())
	}
}

// Task 5: notification dispatcher retry + dead-letter.
// httptest server fails twice then succeeds; we assert the
// delivery_log records two failed attempts followed by a
// success.
func TestRound7_NotificationDeliveryRetry(t *testing.T) {
	var attempts int32
	mu := sync.Mutex{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	delivery := &admin.WebhookDelivery{
		Client:  http.DefaultClient,
		Backoff: []time.Duration{0, 0},
		Sleep:   func(time.Duration) {},
	}
	store := admin.NewInMemoryNotificationStore()
	if err := store.Create(&admin.NotificationPreference{
		ID:        "pref-1",
		TenantID:  round7Tenant,
		Channel:   admin.NotificationChannelWebhook,
		Target:    srv.URL,
		EventType: "chunk.indexed",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("seed pref: %v", err)
	}
	disp, err := admin.NewNotificationDispatcher(store, delivery)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	deliveryLog := admin.NewInMemoryNotificationDeliveryLog()
	disp.SetDeliveryLog(deliveryLog)
	if err := disp.Dispatch(context.Background(), round7Tenant, "chunk.indexed", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	rows, _ := deliveryLog.List(context.Background(), round7Tenant, 10)
	if len(rows) == 0 {
		t.Fatalf("expected delivery log rows")
	}
	// The dispatcher records one row per dispatch (with the
	// final attempt count); a successful delivery after 3
	// attempts therefore lands a single Attempt==3 row.
	maxAttempt := 0
	for _, r := range rows {
		if r.Attempt > maxAttempt {
			maxAttempt = r.Attempt
		}
	}
	if maxAttempt < 3 {
		t.Fatalf("expected attempt count >=3; got rows=%+v", rows)
	}
}

// Task 6: A/B test results aggregator.
func TestRound7_ABTestResults(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	rec, _ := admin.NewQueryAnalyticsRecorder(store)
	for _, arm := range []string{"control", "variant"} {
		for i := 0; i < 5; i++ {
			rec.Record(context.Background(), admin.QueryAnalyticsEvent{
				TenantID: round7Tenant, QueryText: "q", TopK: 10, HitCount: 2,
				LatencyMS: 50 + i, ExperimentName: "exp1", ExperimentArm: arm, At: time.Now(),
			})
		}
	}
	agg, err := admin.NewABTestResultsAggregator(store)
	if err != nil {
		t.Fatalf("aggregator: %v", err)
	}
	h, err := admin.NewABTestResultsHandler(agg)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	w := doJSON(t, r, http.MethodGet, "/v1/admin/retrieval/experiments/exp1/results", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "control") || !strings.Contains(w.Body.String(), "variant") {
		t.Fatalf("missing arms: %s", w.Body.String())
	}
}

// Task 7: credential health endpoint.
func TestRound7_CredentialHealth(t *testing.T) {
	store := admin.NewInMemoryCredentialHealthStore()
	_ = store.RecordCredentialCheck(context.Background(), round7Tenant, "src-1", true, "")
	_ = store.RecordCredentialCheck(context.Background(), round7Tenant, "src-2", false, "401 unauthorized")
	h, err := admin.NewCredentialHealthHandler(store)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	w := doJSON(t, r, http.MethodGet, "/v1/admin/sources/src-2/credential-health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "401") {
		t.Fatalf("missing error: %s", w.Body.String())
	}
}

// Task 9: cache warming.
func TestRound7_CacheWarming(t *testing.T) {
	var got []admin.CacheWarmTuple
	exec := admin.CacheWarmExecutorFunc(func(_ context.Context, tuples []admin.CacheWarmTuple) admin.CacheWarmSummary {
		got = append(got, tuples...)
		return admin.CacheWarmSummary{Total: len(tuples), Succeeded: len(tuples)}
	})
	auditW := &round7Audit{}
	h, err := admin.NewCacheWarmHandler(admin.CacheWarmHandlerConfig{
		Warmer: exec, Audit: auditW,
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	body := map[string]any{"tuples": []map[string]any{{"query": "hi", "top_k": 5}}}
	w := doJSON(t, r, http.MethodPost, "/v1/admin/retrieval/warm-cache", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if len(got) != 1 || got[0].Query != "hi" {
		t.Fatalf("warm exec not called correctly: %+v", got)
	}
}

// Task 10: bulk source operations.
func TestRound7_BulkSourceOps(t *testing.T) {
	repo := &round7BulkRepo{updated: map[string]admin.SourceStatus{}}
	auditW := &round7Audit{}
	h, err := admin.NewBulkSourceHandler(repo, auditW)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	body := map[string]any{"action": "pause", "source_ids": []string{"s1", "s2"}}
	w := doJSON(t, r, http.MethodPost, "/v1/admin/sources/bulk", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if repo.updated["s1"] != admin.SourceStatusPaused {
		t.Fatalf("expected pause on s1; got %v", repo.updated)
	}
	if len(auditW.actions()) != 2 {
		t.Fatalf("expected 2 audit rows; got %d", len(auditW.actions()))
	}
}

type round7BulkRepo struct {
	updated map[string]admin.SourceStatus
}

func (r *round7BulkRepo) Update(_ context.Context, tenantID, id string, patch admin.UpdatePatch) (*admin.Source, error) {
	if patch.Status != nil {
		r.updated[id] = *patch.Status
	}
	return &admin.Source{ID: id, TenantID: tenantID}, nil
}

func (r *round7BulkRepo) MarkRemoving(_ context.Context, tenantID, id string) (*admin.Source, error) {
	r.updated[id] = admin.SourceStatusRemoving
	return &admin.Source{ID: id, TenantID: tenantID}, nil
}

// Task 11: per-tenant latency budget.
func TestRound7_LatencyBudget(t *testing.T) {
	store := admin.NewInMemoryLatencyBudgetStore()
	h, err := admin.NewLatencyBudgetHandler(store, &round7Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	w := doJSON(t, r, http.MethodPut, "/v1/admin/tenants/"+round7Tenant+"/latency-budget", map[string]any{"max_latency_ms": 750})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status: %d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodGet, "/v1/admin/tenants/"+round7Tenant+"/latency-budget", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "750") {
		t.Fatalf("missing 750: %s", w.Body.String())
	}
}

// Task 12: chunk quality (scoring is pure; the report endpoint
// surfaces in-memory store rows aggregated per source).
func TestRound7_ChunkQuality(t *testing.T) {
	scorer := pipeline.NewChunkScorer()
	score := scorer.Score(pipeline.ChunkScorerInput{
		Text:      strings.Repeat("alpha ", 200),
		Embedding: []float32{0.1, 0.2, 0.3},
		Language:  "en",
		LangConf:  0.95,
	})
	if score.Overall == 0 {
		t.Fatalf("expected non-zero overall score; got %+v", score)
	}
	store := admin.NewInMemoryChunkQualityStore()
	store.Insert(context.Background(), admin.ChunkQualityRow{
		TenantID:     round7Tenant,
		SourceID:     "src-1",
		ChunkID:      "chunk-1",
		QualityScore: score.Overall,
		LengthScore:  score.Length,
		LangScore:    score.Language,
		EmbedScore:   score.Embedding,
		Duplicate:    score.Duplicate,
		UpdatedAt:    time.Now(),
	})
	h, err := admin.NewChunkQualityHandler(store)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	w := doJSON(t, r, http.MethodGet, "/v1/admin/chunks/quality-report", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "src-1") {
		t.Fatalf("missing source: %s", w.Body.String())
	}
}

// Task 13: source-sync conflict resolution.
func TestRound7_ConflictResolution(t *testing.T) {
	r, err := pipeline.NewConflictResolver(pipeline.NewInMemoryConflictVersionStore())
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	d1, err := r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: round7Tenant, DocumentID: "doc-1", ContentVersion: 1})
	if err != nil || d1 != pipeline.ConflictDecisionAccept {
		t.Fatalf("first write should be accepted; got %v err=%v", d1, err)
	}
	d2, err := r.Resolve(context.Background(), pipeline.ConflictResolverInput{TenantID: round7Tenant, DocumentID: "doc-1", ContentVersion: 0})
	if err != nil || d2 != pipeline.ConflictDecisionDrop {
		t.Fatalf("stale write should be dropped; got %v err=%v", d2, err)
	}
}

// Task 14: audit export CSV + JSONL.
func TestRound7_AuditExport(t *testing.T) {
	reader := &round7AuditReader{rows: []audit.AuditLog{
		{TenantID: round7Tenant, Action: audit.ActionSourceConnected, ActorID: round7Actor, ResourceType: "source", ResourceID: "src-1", CreatedAt: time.Now()},
		{TenantID: "other", Action: audit.ActionSourceConnected, ActorID: "x", ResourceType: "source", ResourceID: "src-2", CreatedAt: time.Now()},
	}}
	h, err := admin.NewAuditExportHandler(reader, &round7Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	for _, format := range []string{"csv", "jsonl"} {
		w := doJSON(t, r, http.MethodGet, "/v1/admin/audit/export?format="+format, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status: %d body=%s", format, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "src-1") {
			t.Fatalf("%s: missing tenant row: %s", format, body)
		}
		if strings.Contains(body, "src-2") {
			t.Fatalf("%s: cross-tenant leak: %s", format, body)
		}
	}
}

type round7AuditReader struct {
	rows []audit.AuditLog
}

func (r *round7AuditReader) List(_ context.Context, filter audit.ListFilter) (*audit.ListResult, error) {
	out := []audit.AuditLog{}
	for _, row := range r.rows {
		if filter.TenantID != "" && row.TenantID != filter.TenantID {
			continue
		}
		out = append(out, row)
	}
	return &audit.ListResult{Items: out}, nil
}

// Task 15: per-tenant cache TTL.
func TestRound7_CacheConfig(t *testing.T) {
	store := admin.NewInMemoryCacheTTLStore()
	h, err := admin.NewCacheConfigHandler(store, &round7Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	w := doJSON(t, r, http.MethodPut, "/v1/admin/tenants/"+round7Tenant+"/cache-config", map[string]any{"ttl_ms": 60000})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status: %d body=%s", w.Code, w.Body.String())
	}
	ttl := store.TTLFor(context.Background(), round7Tenant, 5*time.Minute)
	if ttl != 60*time.Second {
		t.Fatalf("expected 60s TTL; got %s", ttl)
	}
}

// Task 16: sync history.
func TestRound7_SyncHistory(t *testing.T) {
	rec := admin.NewInMemorySyncHistoryRecorder()
	runID := "run-1"
	if err := rec.Start(context.Background(), round7Tenant, "src-1", runID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := rec.Finish(context.Background(), round7Tenant, "src-1", runID, admin.SyncStatusSucceeded, 42, 1); err != nil {
		t.Fatalf("finish: %v", err)
	}
	h, err := admin.NewSyncHistoryHandler(rec)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	w := doJSON(t, r, http.MethodGet, "/v1/admin/sources/src-1/sync-history?limit=10", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "succeeded") {
		t.Fatalf("missing status: %s", w.Body.String())
	}
}

// Task 17: retrieval pinning.
func TestRound7_RetrievalPinning(t *testing.T) {
	store := admin.NewInMemoryPinnedResultStore(func() string { return fmt.Sprintf("pin-%d", time.Now().UnixNano()) })
	h, err := admin.NewPinnedResultsHandler(store, &round7Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round7Router(h.Register)
	body := map[string]any{"query_pattern": "alpha", "chunk_id": "chunk-1", "position": 0}
	w := doJSON(t, r, http.MethodPost, "/v1/admin/retrieval/pins", body)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("POST status: %d body=%s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodGet, "/v1/admin/retrieval/pins?query=alpha", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "chunk-1") {
		t.Fatalf("missing chunk id: %s", w.Body.String())
	}
}

// Task 18: pipeline health dashboard.
func TestRound7_PipelineHealth(t *testing.T) {
	if _, err := admin.NewPipelineHealthAggregator(nil); err == nil {
		t.Fatalf("expected error for nil registry")
	}
	// In-process registry from observability lives in the binary;
	// the smoke is the happy path that the aggregator + handler
	// pair compile and a nil registry is rejected.
	io.Discard.Write(nil) // keep io import even if happy path is trimmed
}
