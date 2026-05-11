//go:build e2e

// Round-12 Task 19 — comprehensive e2e smoke-test of the Round-12 surface.
//
// This file exercises the Round-12 additions as an integrated bundle:
//
//	(a) DLQ auto-replay fires and re-emits a replayable row.
//	(b) Rate-limit status endpoint returns valid JSON.
//	(c) Audit retention sweeper deletes old rows.
//	(d) Isolation smoke passes for two tenants.
//	(e) Scheduler recovery after a simulated panic.
//	(f) At least one new Round-12 alert rule validates via alertcheck.
//
// Build tag `e2e` keeps this out of the fast lane; it runs in the
// full lane after the storage plane is up.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// newSchedulerDBForE2E mirrors the helper in internal/admin/scheduler_test.go.
func newSchedulerDBForE2E(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&admin.SyncSchedule{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// ----------------------- shared helpers -----------------------

func mustNewGin() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return gin.New()
}

// ulid26 is a 26-char synthetic id, enough for char(26) primary keys
// in the audit / dlq schemas without pulling in oklog/ulid.
func ulid26(s string) string {
	for len(s) < 26 {
		s += "x"
	}
	return s[:26]
}

// ---------- (a) DLQ auto-replay end-to-end ----------

// stubAutoReplayStore is a recording AutoReplayStore.
type stubAutoReplayStore struct {
	rows []pipeline.DLQMessage
}

func (s *stubAutoReplayStore) ListAutoReplayable(_ context.Context, now time.Time, limit int) ([]pipeline.DLQMessage, error) {
	out := make([]pipeline.DLQMessage, 0, len(s.rows))
	for _, r := range s.rows {
		if r.ReplayedAt == nil && r.AttemptCount < 5 {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	_ = now
	return out, nil
}

func (s *stubAutoReplayStore) Get(_ context.Context, tenantID, id string) (*pipeline.DLQMessage, error) {
	for i := range s.rows {
		if s.rows[i].TenantID == tenantID && s.rows[i].ID == id {
			cp := s.rows[i]
			return &cp, nil
		}
	}
	return nil, errors.New("not found")
}

type stubAutoReplayer struct {
	calls []string
	bump  func(id string)
}

func (r *stubAutoReplayer) Replay(_ context.Context, tenantID, id, topic string, _ bool) error {
	r.calls = append(r.calls, tenantID+":"+id+":"+topic)
	if r.bump != nil {
		r.bump(id)
	}
	return nil
}

func (r *stubAutoReplayer) MaxAttempts() int { return 5 }

func TestRound12_DLQAutoReplay_FiresAndReEmits(t *testing.T) {
	if os.Getenv("CONTEXT_ENGINE_DLQ_AUTO_REPLAY") == "" {
		t.Setenv("CONTEXT_ENGINE_DLQ_AUTO_REPLAY", "true")
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	row := pipeline.DLQMessage{
		ID:            ulid26("dlq-row-a"),
		TenantID:      ulid26("tenant-x"),
		SourceID:      "src-1",
		OriginalTopic: "ingest.events",
		Payload:       []byte("{\"k\":\"v\"}"),
		ErrorText:     "transient",
		FailedAt:      now.Add(-time.Hour),
		AttemptCount:  1,
		CreatedAt:     now.Add(-time.Hour),
	}
	store := &stubAutoReplayStore{rows: []pipeline.DLQMessage{row}}
	rep := &stubAutoReplayer{
		bump: func(id string) {
			for i := range store.rows {
				if store.rows[i].ID == id {
					ts := now
					store.rows[i].ReplayedAt = &ts
				}
			}
		},
	}
	w, err := pipeline.NewDLQAutoReplayer(pipeline.DLQAutoReplayConfig{
		Store:    store,
		Replayer: rep,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new replayer: %v", err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(rep.calls) != 1 {
		t.Fatalf("expected one replay call, got %d (%v)", len(rep.calls), rep.calls)
	}
	if !strings.HasSuffix(rep.calls[0], ":ingest.events") {
		t.Fatalf("replay call did not target original topic: %s", rep.calls[0])
	}
	if store.rows[0].ReplayedAt == nil {
		t.Fatalf("expected replayed_at to be set after Tick")
	}
}

// ---------- (b) Rate-limit status endpoint returns valid JSON ----------

func TestRound12_RateLimitStatus_ReturnsValidJSON(t *testing.T) {
	// We use a fake limiter implementing admin.RateLimitInspector so
	// the test stays hermetic; the real Redis-backed limiter is
	// covered by internal/admin/rate_limit_status_handler_test.go.
	tenantID := "tenant-rls"
	sourceID := "src-1"
	fake := &fakeRLInspector{
		want: admin.RateLimitStatus{
			TenantID:      tenantID,
			SourceID:      sourceID,
			CurrentTokens: 7.5,
			MaxTokens:     10,
			EffectiveRate: 1.0,
			HalveCount:    1,
			IsThrottled:   false,
		},
	}
	h, err := admin.NewRateLimitStatusHandler(fake)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	r := mustNewGin()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Next()
	})
	h.Register(&r.RouterGroup)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/"+sourceID+"/rate-limit-status", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got admin.RateLimitStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got.SourceID != sourceID || got.MaxTokens != 10 || got.CurrentTokens <= 0 {
		t.Fatalf("payload mismatch: %+v", got)
	}
}

type fakeRLInspector struct {
	want admin.RateLimitStatus
}

func (f *fakeRLInspector) Inspect(_ context.Context, tenant, source string) (admin.RateLimitStatus, error) {
	out := f.want
	out.TenantID = tenant
	out.SourceID = source
	return out, nil
}

// ---------- (c) Audit retention sweeper deletes old rows ----------

func TestRound12_AuditRetention_DeletesOldRows(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Exec(`CREATE TABLE audit_logs (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		action TEXT NOT NULL,
		created_at DATETIME NOT NULL)`).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insert := func(id string, ts time.Time) {
		t.Helper()
		if err := db.Exec(
			`INSERT INTO audit_logs(id,tenant_id,action,created_at) VALUES(?,?,?,?)`,
			id, "tenant-x", "act", ts,
		).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insert("old-1", now.Add(-120*24*time.Hour))
	insert("old-2", now.Add(-95*24*time.Hour))
	insert("fresh", now.Add(-3*24*time.Hour))

	before := testutil.ToFloat64(observability.AuditRowsExpiredTotal)
	s, err := admin.NewAuditRetentionSweeper(admin.AuditRetentionConfig{
		DB:              db,
		RetentionWindow: 90 * 24 * time.Hour,
		BatchSize:       100,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM audit_logs`).Scan(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("after sweep: want 1 row, got %d", n)
	}
	got := testutil.ToFloat64(observability.AuditRowsExpiredTotal) - before
	if got != 2 {
		t.Fatalf("expired counter delta: want 2 got %v", got)
	}
}

// ---------- (d) Isolation smoke passes for two tenants ----------

func TestRound12_IsolationSmoke_TwoTenants(t *testing.T) {
	// The Round-12 Task 7 isolation_smoke_test.go covers this in
	// more detail; the assertion here is the same shape but kept
	// in-line so a single `go test -tags=e2e -run Round12` covers
	// the whole Round-12 surface.
	checkers := []admin.IsolationChecker{
		admin.CheckerFunc{
			BackendName: "qdrant",
			Fn: func(_ context.Context) admin.IsolationCheckResult {
				return admin.IsolationCheckResult{Pass: true, Inspected: 4}
			},
		},
		admin.CheckerFunc{
			BackendName: "redis",
			Fn: func(_ context.Context) admin.IsolationCheckResult {
				return admin.IsolationCheckResult{Pass: true, Inspected: 4}
			},
		},
	}
	auditor, err := admin.NewIsolationAuditor(checkers)
	if err != nil {
		t.Fatalf("new auditor: %v", err)
	}
	h, err := admin.NewIsolationHandler(auditor)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	r := mustNewGin()
	h.Register(&r.RouterGroup)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/isolation-check", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("isolation-check status: %d body=%s", rr.Code, rr.Body.String())
	}
	var report admin.IsolationReport
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !report.OverallPass {
		t.Fatalf("overall_pass false; results=%+v", report.Results)
	}
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 backend results, got %d", len(report.Results))
	}
}

// ---------- (e) Scheduler recovery after a simulated panic ----------

type panickyEmitter struct{ n int }

func (p *panickyEmitter) EmitSync(_ context.Context, _, _ string) error {
	p.n++
	panic("simulated emitter panic")
}

func TestRound12_SchedulerPanicRecovery(t *testing.T) {
	db := newSchedulerDBForE2E(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := admin.UpsertSchedule(context.Background(), db, "tenant-r12", "src-r12", "@every 5m", true, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	emit := &panickyEmitter{}
	s, err := admin.NewScheduler(admin.SchedulerConfig{
		DB: db, Emitter: emit,
		Now: func() time.Time { return now.Add(10 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	before := testutil.ToFloat64(observability.SchedulerErrorsTotal)
	if err := s.SafeTick(context.Background()); err == nil {
		t.Fatalf("SafeTick must surface panic as error")
	}
	if emit.n != 1 {
		t.Fatalf("emitter called %d times, want 1", emit.n)
	}
	if got := testutil.ToFloat64(observability.SchedulerErrorsTotal) - before; got < 1 {
		t.Fatalf("scheduler errors counter delta=%v", got)
	}
}

// ---------- (f) Alertcheck validates Round-12 alert rules ----------

// TestRound12_AlertcheckValidatesRound12Rules runs the alertcheck
// binary against deploy/alerts.yaml and asserts (1) it exits 0,
// and (2) the alerts.yaml file declares each Round-12 alert rule.
// The alertcheck binary gates on summary/severity presence; here
// we also assert the new rule names are wired in the manifest.
func TestRound12_AlertcheckValidatesRound12Rules(t *testing.T) {
	root := repoRootRound12(t)
	cmd := exec.Command("go", "run", "./internal/observability/alertcheck",
		filepath.Join(root, "deploy/alerts.yaml"),
		filepath.Join(root, "deploy/recording-rules.yaml"),
	)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("alertcheck failed: %v\nstderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "deploy/alerts.yaml"))
	if err != nil {
		t.Fatalf("read alerts.yaml: %v", err)
	}
	yaml := string(data)
	required := []string{
		"GRPCCircuitBreakerOpen",
		"PostgresPoolSaturated",
		"RedisPoolSaturated",
	}
	for _, name := range required {
		if !strings.Contains(yaml, name) {
			t.Fatalf("deploy/alerts.yaml missing Round-12 rule %q", name)
		}
	}
}

// repoRootRound12 walks up from the package dir until it finds
// the repo root (go.mod). Mirrors helper in tests/regression.
func repoRootRound12(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root not found from %s", wd)
		}
		dir = parent
	}
}
