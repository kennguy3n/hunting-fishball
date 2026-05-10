package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

const sqliteHealthSchemaForDashboard = `
CREATE TABLE source_health (
    tenant_id        TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    last_success_at  DATETIME,
    last_failure_at  DATETIME,
    lag              INTEGER NOT NULL DEFAULT 0,
    error_count      INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'unknown',
    updated_at       DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, source_id)
);`

func newDashboardDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.Exec(sqliteHealthSchemaForDashboard).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

type fakeMetrics struct {
	throughput float64
	p95        float64
	avail      map[string]bool
	err        error
}

func (f fakeMetrics) PipelineThroughputDocsPerMin(_ context.Context, _ string) (float64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.throughput, nil
}

func (f fakeMetrics) RetrievalP95Ms(_ context.Context, _ string) (float64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.p95, nil
}

func (f fakeMetrics) BackendAvailability(_ context.Context) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.avail, nil
}

func seedHealth(t *testing.T, db *gorm.DB, tenantID, sourceID string, status admin.HealthStatus, lag, errs int) {
	t.Helper()
	row := admin.Health{
		TenantID:   tenantID,
		SourceID:   sourceID,
		Status:     status,
		Lag:        lag,
		ErrorCount: errs,
		UpdatedAt:  time.Now().UTC(),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func newDashboardRouter(t *testing.T, db *gorm.DB, metrics admin.MetricsSnapshot, tenantID string) *gin.Engine {
	t.Helper()
	repo := admin.NewHealthRepository(db, admin.HealthThresholds{})
	h, err := admin.NewDashboardHandler(repo, metrics)
	if err != nil {
		t.Fatalf("NewDashboardHandler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, tenantID); c.Next() })
	rg := r.Group("/")
	h.Register(rg)
	return r
}

func TestDashboard_AggregatesByStatus(t *testing.T) {
	t.Parallel()
	db := newDashboardDB(t)
	seedHealth(t, db, "t-a", "s-1", admin.HealthStatusHealthy, 0, 0)
	seedHealth(t, db, "t-a", "s-2", admin.HealthStatusDegraded, 5, 1)
	seedHealth(t, db, "t-a", "s-3", admin.HealthStatusFailing, 50, 9)
	seedHealth(t, db, "t-a", "s-4", admin.HealthStatusUnknown, 0, 0)
	seedHealth(t, db, "t-b", "s-99", admin.HealthStatusFailing, 99, 99) // tenant isolation
	r := newDashboardRouter(t, db, nil, "t-a")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var out admin.DashboardSummary
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.HealthyCount != 1 || out.DegradedCount != 1 || out.FailingCount != 1 || out.UnknownCount != 1 {
		t.Fatalf("counts = %+v", out)
	}
	if len(out.Connectors) != 4 {
		t.Fatalf("connectors=%d (tenant leak?)", len(out.Connectors))
	}
}

func TestDashboard_RequiresTenant(t *testing.T) {
	t.Parallel()
	db := newDashboardDB(t)
	repo := admin.NewHealthRepository(db, admin.HealthThresholds{})
	h, _ := admin.NewDashboardHandler(repo, nil)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestDashboard_IncludesMetricsWhenProvided(t *testing.T) {
	t.Parallel()
	db := newDashboardDB(t)
	seedHealth(t, db, "t-a", "s-1", admin.HealthStatusHealthy, 0, 0)
	m := fakeMetrics{
		throughput: 12.5,
		p95:        88,
		avail:      map[string]bool{"vector": true, "bm25": true, "graph": false},
	}
	r := newDashboardRouter(t, db, m, "t-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var out admin.DashboardSummary
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.ThroughputDocsPMin != 12.5 || out.RetrievalP95Ms != 88 {
		t.Fatalf("metrics not propagated: %+v", out)
	}
	if !out.BackendAvailability["vector"] || out.BackendAvailability["graph"] {
		t.Fatalf("availability = %+v", out.BackendAvailability)
	}
}

func TestDashboard_MetricsErrorsAreSwallowed(t *testing.T) {
	t.Parallel()
	db := newDashboardDB(t)
	seedHealth(t, db, "t-a", "s-1", admin.HealthStatusHealthy, 0, 0)
	m := fakeMetrics{err: errors.New("scrape failed")}
	r := newDashboardRouter(t, db, m, "t-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestDashboard_NewRejectsNilHealth(t *testing.T) {
	t.Parallel()
	if _, err := admin.NewDashboardHandler(nil, nil); err == nil {
		t.Fatalf("expected error for nil health repo")
	}
}
