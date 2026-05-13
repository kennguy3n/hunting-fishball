package admin_test

import (
	"context"
	"encoding/json"
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

// connectorHealthSchema is the SQLite mirror of the two tables
// the handler joins: sources + source_health. Mirrors
// sqliteSourcesSchema + sqliteHealthSchemaForDashboard.
const connectorHealthSchema = `
CREATE TABLE sources (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    connector_type  TEXT NOT NULL,
    config          TEXT NOT NULL DEFAULT '{}',
    scopes          TEXT NOT NULL DEFAULT '[]',
    status          TEXT NOT NULL DEFAULT 'active',
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
);
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
);
`

func newConnectorHealthDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.Exec(connectorHealthSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func seedConnectorSource(t *testing.T, repo *admin.SourceRepository, tenantID, id, ctype string, status admin.SourceStatus) {
	t.Helper()
	s := &admin.Source{
		ID:            id,
		TenantID:      tenantID,
		ConnectorType: ctype,
		Status:        status,
		Config:        admin.JSONMap{},
		Scopes:        []string{},
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), s); err != nil {
		t.Fatalf("seed source: %v", err)
	}
}

func seedConnectorHealth(t *testing.T, db *gorm.DB, tenantID, sourceID string, status admin.HealthStatus, lag, errs int) {
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
		t.Fatalf("seed health: %v", err)
	}
}

func TestConnectorHealthHandler_NilRepos(t *testing.T) {
	t.Parallel()
	if _, err := admin.NewConnectorHealthHandler(nil, nil); err == nil {
		t.Fatalf("expected error on nil sources")
	}
	db := newConnectorHealthDB(t)
	srcRepo := admin.NewSourceRepository(db)
	if _, err := admin.NewConnectorHealthHandler(srcRepo, nil); err == nil {
		t.Fatalf("expected error on nil health repo")
	}
}

func TestConnectorHealthHandler_AggregatesByConnectorType(t *testing.T) {
	t.Parallel()
	db := newConnectorHealthDB(t)
	srcRepo := admin.NewSourceRepository(db)
	healthRepo := admin.NewHealthRepository(db, admin.HealthThresholds{})
	const tenantID = "tenant-a"

	// Two zendesk sources: 1 healthy, 1 failing.
	seedConnectorSource(t, srcRepo, tenantID, "src-z1", "zendesk", admin.SourceStatusActive)
	seedConnectorSource(t, srcRepo, tenantID, "src-z2", "zendesk", admin.SourceStatusActive)
	seedConnectorHealth(t, db, tenantID, "src-z1", admin.HealthStatusHealthy, 10, 0)
	seedConnectorHealth(t, db, tenantID, "src-z2", admin.HealthStatusFailing, 200, 30)

	// One bitbucket source, paused.
	seedConnectorSource(t, srcRepo, tenantID, "src-b1", "bitbucket", admin.SourceStatusPaused)

	// One airtable source, active but no health row yet.
	seedConnectorSource(t, srcRepo, tenantID, "src-a1", "airtable", admin.SourceStatusActive)

	h, err := admin.NewConnectorHealthHandler(srcRepo, healthRepo)
	if err != nil {
		t.Fatalf("NewConnectorHealthHandler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, tenantID); c.Next() })
	h.Register(r.Group("/"))

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/connectors/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var got admin.ConnectorHealthSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.TenantID != tenantID {
		t.Fatalf("tenant: %q", got.TenantID)
	}
	if got.Total != 4 {
		t.Fatalf("total sources: %d want 4", got.Total)
	}
	if len(got.Connectors) != 3 {
		t.Fatalf("connector_types: %d want 3", len(got.Connectors))
	}
	byType := map[string]admin.ConnectorTypeHealth{}
	for _, c := range got.Connectors {
		byType[c.ConnectorType] = c
	}
	if z := byType["zendesk"]; z.Total != 2 || z.Healthy != 1 || z.Failing != 1 || z.AvgLag != 105 || z.AvgErrorCount != 15 {
		t.Fatalf("zendesk row mismatch: %+v", z)
	}
	if z := byType["zendesk"]; z.ErrorRate < 0.49 || z.ErrorRate > 0.51 {
		t.Fatalf("zendesk error_rate: %f want ~0.5", z.ErrorRate)
	}
	if b := byType["bitbucket"]; b.Total != 1 || b.Paused != 1 || b.Healthy != 0 {
		t.Fatalf("bitbucket row mismatch: %+v", b)
	}
	if a := byType["airtable"]; a.Total != 1 || a.Unknown != 1 {
		t.Fatalf("airtable row mismatch: %+v", a)
	}
}

func TestConnectorHealthHandler_MissingTenantContext(t *testing.T) {
	t.Parallel()
	db := newConnectorHealthDB(t)
	srcRepo := admin.NewSourceRepository(db)
	healthRepo := admin.NewHealthRepository(db, admin.HealthThresholds{})
	h, _ := admin.NewConnectorHealthHandler(srcRepo, healthRepo)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r.Group("/"))
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/connectors/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d want 401", w.Code)
	}
}
