package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// tenantPayloadLimitSQLiteDDL mirrors
// migrations/039_tenant_payload_limits.sql with SQLite-compatible
// types (DATETIME in place of TIMESTAMPTZ, CURRENT_TIMESTAMP for
// the default). AutoMigrate can't be used because the GORM tag
// pins `default:now()` which SQLite rejects.
const tenantPayloadLimitSQLiteDDL = `CREATE TABLE IF NOT EXISTS tenant_payload_limits (
tenant_id  CHAR(26)  NOT NULL PRIMARY KEY,
max_bytes  BIGINT    NOT NULL,
updated_at DATETIME  NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func newTenantPayloadLimitDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.Exec(tenantPayloadLimitSQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestTenantPayloadLimitStoreGORM_CRUD(t *testing.T) {
	db := newTenantPayloadLimitDB(t)
	store := admin.NewTenantPayloadLimitStoreGORM(db)
	ctx := context.Background()

	cap, ok, err := store.Get(ctx, "t-1")
	if err != nil || ok {
		t.Fatalf("Get miss: cap=%d ok=%v err=%v", cap, ok, err)
	}

	if err := store.Upsert(ctx, "t-1", 4096); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	cap, ok, err = store.Get(ctx, "t-1")
	if err != nil || !ok || cap != 4096 {
		t.Fatalf("Get after insert: cap=%d ok=%v err=%v", cap, ok, err)
	}

	if err := store.Upsert(ctx, "t-1", 8192); err != nil {
		t.Fatalf("Upsert overwrite: %v", err)
	}
	cap, ok, _ = store.Get(ctx, "t-1")
	if !ok || cap != 8192 {
		t.Fatalf("Get after overwrite: cap=%d", cap)
	}

	if err := store.Delete(ctx, "t-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, _ = store.Get(ctx, "t-1")
	if ok {
		t.Fatalf("Get after delete should miss")
	}
}

func TestTenantPayloadLimitStore_RejectsInvalid(t *testing.T) {
	store := admin.NewInMemoryTenantPayloadLimitStore()
	if err := store.Upsert(context.Background(), "", 1); err == nil {
		t.Fatalf("empty tenant should fail")
	}
	if err := store.Upsert(context.Background(), "t", 0); err == nil {
		t.Fatalf("zero cap should fail")
	}
}

func TestTenantPayloadLookup_AppliesPerTenantOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := admin.NewInMemoryTenantPayloadLimitStore()
	if err := store.Upsert(context.Background(), "tenant-a", 1024); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	cfg := observability.PayloadLimiterConfig{
		MaxBytes:       64 * 1024,
		TenantOverride: admin.TenantPayloadLookup(store),
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "tenant-a") })
	r.Use(observability.PayloadSizeLimiter(cfg))
	r.POST("/echo", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Body larger than per-tenant cap but smaller than global cap.
	body := strings.NewReader(strings.Repeat("x", 4096))
	req := httptest.NewRequest(http.MethodPost, "/echo", body)
	req.Header.Set("Content-Length", "4096")
	req.ContentLength = 4096
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d body %s want 413", w.Code, w.Body.String())
	}
}

func TestTenantPayloadLookup_FallsBackToGlobal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := admin.NewInMemoryTenantPayloadLimitStore()

	cfg := observability.PayloadLimiterConfig{
		MaxBytes:       64 * 1024,
		TenantOverride: admin.TenantPayloadLookup(store),
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "tenant-z") })
	r.Use(observability.PayloadSizeLimiter(cfg))
	r.POST("/echo", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := strings.NewReader(strings.Repeat("x", 4096))
	req := httptest.NewRequest(http.MethodPost, "/echo", body)
	req.ContentLength = 4096
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d body %s want 200 (global cap headroom)", w.Code, w.Body.String())
	}
}
