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

// slowQuerySQLiteDDL mirrors migrations/038_slow_queries.sql but
// uses SQLite-compatible types: TEXT in place of JSONB, no
// TIMESTAMPTZ, and CURRENT_TIMESTAMP instead of now(). JSONMap.
// Scan/Value already handle TEXT round-trips so this stays
// faithful to the production behaviour.
const slowQuerySQLiteDDL = `CREATE TABLE IF NOT EXISTS slow_queries (
id              CHAR(26)    NOT NULL PRIMARY KEY,
tenant_id       CHAR(26)    NOT NULL,
query_hash      VARCHAR(64) NOT NULL,
query_text      TEXT        NOT NULL,
latency_ms      INTEGER     NOT NULL,
top_k           INTEGER     NOT NULL DEFAULT 0,
hit_count       INTEGER     NOT NULL DEFAULT 0,
backend_timings TEXT,
created_at      DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func newSlowQueryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.Exec(slowQuerySQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestSlowQueryStoreGORM_RecordAndList(t *testing.T) {
	db := newSlowQueryDB(t)
	store := admin.NewSlowQueryStoreGORM(db)
	ctx := context.Background()

	tNow := time.Now().UTC()
	rows := []*admin.SlowQueryRow{
		{TenantID: "tenant-a", QueryHash: "h1", QueryText: "a", LatencyMS: 1200, CreatedAt: tNow.Add(-3 * time.Minute)},
		{TenantID: "tenant-a", QueryHash: "h2", QueryText: "b", LatencyMS: 1800, CreatedAt: tNow.Add(-2 * time.Minute)},
		{TenantID: "tenant-a", QueryHash: "h3", QueryText: "c", LatencyMS: 5000, CreatedAt: tNow.Add(-1 * time.Minute)},
		{TenantID: "tenant-b", QueryHash: "h4", QueryText: "other", LatencyMS: 9000, CreatedAt: tNow},
	}
	for _, r := range rows {
		if err := store.RecordSlowQuery(ctx, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	got, err := store.ListSlowQueries(ctx, admin.SlowQueryFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	// newest first
	if got[0].QueryHash != "h3" {
		t.Fatalf("first=%q want h3", got[0].QueryHash)
	}

	// since filter excludes older rows
	since := tNow.Add(-90 * time.Second)
	got, err = store.ListSlowQueries(ctx, admin.SlowQueryFilter{TenantID: "tenant-a", Since: since})
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	if len(got) != 1 || got[0].QueryHash != "h3" {
		t.Fatalf("got=%+v", got)
	}

	// other tenant isolated
	got, err = store.ListSlowQueries(ctx, admin.SlowQueryFilter{TenantID: "tenant-b"})
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tenant isolation: got %+v", got)
	}
}

func TestSlowQueryStoreGORM_RejectsMissingTenant(t *testing.T) {
	db := newSlowQueryDB(t)
	store := admin.NewSlowQueryStoreGORM(db)
	if err := store.RecordSlowQuery(context.Background(), &admin.SlowQueryRow{QueryText: "x"}); err == nil {
		t.Fatalf("expected error for missing tenant_id")
	}
	if _, err := store.ListSlowQueries(context.Background(), admin.SlowQueryFilter{}); err == nil {
		t.Fatalf("expected error for missing tenant_id")
	}
}

func TestInMemorySlowQueryStore_RecordAndList(t *testing.T) {
	store := admin.NewInMemorySlowQueryStore()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := store.RecordSlowQuery(ctx, &admin.SlowQueryRow{TenantID: "t", QueryText: "x", LatencyMS: 100 + i, CreatedAt: time.Now().Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	got, err := store.ListSlowQueries(ctx, admin.SlowQueryFilter{TenantID: "t", Limit: 3})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
}

func TestSlowQueryLogHandler_TenantIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := admin.NewInMemorySlowQueryStore()
	_ = store.RecordSlowQuery(context.Background(), &admin.SlowQueryRow{TenantID: "tenant-a", QueryText: "x", LatencyMS: 1500})
	_ = store.RecordSlowQuery(context.Background(), &admin.SlowQueryRow{TenantID: "tenant-b", QueryText: "y", LatencyMS: 1500})

	h := admin.NewSlowQueryLogHandler(store)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "tenant-a") })
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/retrieval/slow-queries", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp admin.SlowQueryLogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.TenantID != "tenant-a" || len(resp.Items) != 1 {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestSlowQueryLogHandler_NoTenantIsUnauthorized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := admin.NewInMemorySlowQueryStore()
	h := admin.NewSlowQueryLogHandler(store)
	r := gin.New()
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/retrieval/slow-queries", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}
