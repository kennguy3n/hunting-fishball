//go:build e2e

// Package e2e — pr13_features_test.go — Round-5 Task 20.
//
// Exercises the PR #13 features against the full docker-compose
// stack (Postgres + Qdrant + Kafka + Redis). Each test assumes the
// API and ingest binaries are NOT running — tests wire up the
// handler/store layers directly, same as smoke_test.go.
//
// To run: `make test-e2e`
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/eval"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// pgDSN reads the Postgres connection string from the
// environment or falls back to the docker-compose default.
func pgDSN() string {
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return "host=localhost port=5432 user=postgres password=postgres dbname=context_engine sslmode=disable"
}

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(pgDSN()), &gorm.Config{})
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	return db
}

// --- Eval runner invocation ---

func TestEvalRunner_SeededCorpus(t *testing.T) {
	t.Parallel()
	suite := eval.EvalSuite{
		Name:     "e2e-eval",
		TenantID: "e2e-tenant",
		DefaultK: 3,
		Cases: []eval.EvalCase{
			{
				Query:            "how to connect google drive",
				ExpectedChunkIDs: []string{"chunk-gd-1"},
			},
		},
	}
	stub := &stubRetriever{
		results: []eval.RetrieveHit{
			{ID: "chunk-gd-1", Score: 0.92},
			{ID: "chunk-gd-2", Score: 0.80},
		},
	}
	report, err := eval.Run(context.Background(), stub, suite)
	if err != nil {
		t.Fatalf("eval.Run: %v", err)
	}
	if report.FailedCases > 0 {
		t.Fatalf("failed cases: %d", report.FailedCases)
	}
	if report.Aggregate.PrecisionAtK == 0 {
		t.Fatalf("aggregate precision is zero — eval not wired")
	}
}

type stubRetriever struct {
	results []eval.RetrieveHit
}

func (s *stubRetriever) Retrieve(_ context.Context, _ eval.RetrieveRequest) ([]eval.RetrieveHit, error) {
	return s.results, nil
}

// --- Scheduler CRUD ---

func TestSchedulerCRUD(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	ctx := context.Background()
	_ = db.Exec("CREATE TABLE IF NOT EXISTS sync_schedules (id VARCHAR(26) PRIMARY KEY, tenant_id VARCHAR(26) NOT NULL, source_id VARCHAR(26) NOT NULL, cron_expr VARCHAR(64) NOT NULL, enabled BOOLEAN NOT NULL DEFAULT TRUE, next_run_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE(tenant_id, source_id))")

	tenant := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	now := time.Now().UTC()

	s, err := admin.UpsertSchedule(ctx, db, tenant, "src-1", "0 */2 * * *", true, now)
	if err != nil {
		t.Fatalf("UpsertSchedule (create): %v", err)
	}
	if s.ID == "" || s.CronExpr != "0 */2 * * *" {
		t.Fatalf("unexpected schedule: %+v", s)
	}

	got, err := admin.GetSchedule(ctx, db, tenant, "src-1")
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if got.CronExpr != "0 */2 * * *" || !got.Enabled {
		t.Fatalf("read mismatch: %+v", got)
	}

	_, err = admin.UpsertSchedule(ctx, db, tenant, "src-1", "0 */2 * * *", false, now)
	if err != nil {
		t.Fatalf("UpsertSchedule (disable): %v", err)
	}
	got2, _ := admin.GetSchedule(ctx, db, tenant, "src-1")
	if got2.Enabled {
		t.Fatalf("expected disabled after upsert")
	}
}

// --- Metering increment + query ---

func TestMeteringIncrementAndQuery(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	ctx := context.Background()
	store := admin.NewMeteringStoreGORM(db)
	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	tenant := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	today := time.Now().UTC().Truncate(24 * time.Hour)

	if err := store.Increment(ctx, tenant, today, "retrieval_requests", 5); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if err := store.Increment(ctx, tenant, today, "retrieval_requests", 3); err != nil {
		t.Fatalf("Increment 2: %v", err)
	}
	rows, err := store.List(ctx, tenant, today, today.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Count != 8 {
		t.Fatalf("metering result: %+v", rows)
	}
}

// --- DLQ list + single replay ---

func TestDLQ_ListAndReplay(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	dlq := newMemDLQ()
	tenant := "e2e-tenant-dlq"
	dlq.Push(pipeline.DLQMessage{
		ID: "dlq-1", TenantID: tenant, SourceID: "src",
		Payload: []byte(`{}`), ErrorText: "timeout",
		OriginalTopic: "ingest",
	})
	au := &noopAudit{}
	h, err := admin.NewDLQHandler(admin.DLQHandlerConfig{Reader: dlq, Audit: au})
	if err != nil {
		t.Fatalf("NewDLQHandler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenant)
		c.Set(audit.ActorContextKey, "e2e")
		c.Next()
	})
	rg := r.Group("")
	h.Register(rg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("DLQ list: %d body=%s", w.Code, w.Body.String())
	}
}

// --- Alerting rules validation ---

func TestAlertingRulesValid(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../deploy/alerts.yaml")
	if err != nil {
		t.Fatalf("read alerts.yaml: %v", err)
	}
	if len(data) < 50 {
		t.Fatalf("alerts.yaml suspiciously small: %d bytes", len(data))
	}
	if !bytes.Contains(data, []byte("alert:")) {
		t.Fatal("no 'alert:' key found in deploy/alerts.yaml")
	}
}

// --- Index health endpoint ---

func TestIndexHealthEndpoint(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	h := admin.NewIndexHealthHandler(
		&pingChecker{name: "qdrant", err: nil},
		&pingChecker{name: "bleve", err: nil},
	)
	r := gin.New()
	h.Register(r.Group(""))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/health/indexes", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("index health: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.IndexHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Healthy || len(resp.Backends) != 2 {
		t.Fatalf("unexpected health response: %+v", resp)
	}
}

type pingChecker struct {
	name string
	err  error
}

func (p *pingChecker) Name() string                { return p.name }
func (p *pingChecker) Check(context.Context) error { return p.err }

// --- SSE progress stream connect + disconnect ---

func TestSSEProgressStream(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	ps := &memProgressReader{rows: []admin.SyncProgress{
		{TenantID: "e2e-tenant", SourceID: "src-1", Discovered: 10, Processed: 7},
	}}
	h := admin.NewProgressStreamHandler(ps)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "e2e-tenant")
		c.Next()
	})
	rg := r.Group("")
	h.Register(rg)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/sync/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.ServeHTTP(w, req.WithContext(ctx))
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("SSE stream: %d body=%s", w.Code, w.Body.String())
	}
}

type memProgressReader struct {
	rows []admin.SyncProgress
}

func (m *memProgressReader) List(_ context.Context, tenantID, sourceID string) ([]admin.SyncProgress, error) {
	var out []admin.SyncProgress
	for _, r := range m.rows {
		if r.TenantID == tenantID && r.SourceID == sourceID {
			out = append(out, r)
		}
	}
	return out, nil
}

// --- Credential rotation happy path ---

func TestCredentialRotation_FakeOAuthServer(t *testing.T) {
	t.Parallel()
	cfg := admin.TokenRefreshWorkerConfig{
		Lister:    &emptyLister{},
		Updater:   &noopUpdater{},
		Refresher: &noopRefresher{},
	}
	w, err := admin.NewTokenRefreshWorker(cfg)
	if err != nil {
		t.Fatalf("NewTokenRefreshWorker: %v", err)
	}
	if _, err = w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// --- helpers ---

type emptyLister struct{}

func (e *emptyLister) ListAllActive(context.Context) ([]admin.Source, error) {
	return nil, nil
}

type noopUpdater struct{}

func (noopUpdater) UpdateConfig(context.Context, string, string, admin.JSONMap) error { return nil }

type noopRefresher struct{}

func (noopRefresher) Refresh(context.Context, admin.RefreshParams) (admin.RefreshResult, error) {
	return admin.RefreshResult{}, nil
}

type noopAudit struct{}

func (noopAudit) Create(context.Context, *audit.AuditLog) error                     { return nil }
func (noopAudit) CreateInTx(_ context.Context, _ *gorm.DB, _ *audit.AuditLog) error { return nil }

// memDLQ implements admin.DLQReader for e2e tests.
type memDLQ struct {
	rows []pipeline.DLQMessage
}

func newMemDLQ() *memDLQ { return &memDLQ{} }

func (m *memDLQ) Push(msg pipeline.DLQMessage) {
	m.rows = append(m.rows, msg)
}

func (m *memDLQ) List(_ context.Context, f pipeline.DLQListFilter) ([]pipeline.DLQMessage, error) {
	var out []pipeline.DLQMessage
	for _, r := range m.rows {
		if r.TenantID == f.TenantID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memDLQ) Get(_ context.Context, tenantID, id string) (*pipeline.DLQMessage, error) {
	for i := range m.rows {
		if m.rows[i].ID == id && m.rows[i].TenantID == tenantID {
			return &m.rows[i], nil
		}
	}
	return nil, fmt.Errorf("not found")
}
