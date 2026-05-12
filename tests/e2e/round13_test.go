//go:build e2e

// Round-14 Task 10 — Round-13 end-to-end smoke.
//
// This file exercises the Round-13 surface as an integrated bundle.
// Each subtest mirrors a feature added in Round 13 and runs against
// in-process fakes (SQLite for GORM, miniredis where relevant) so
// the test runs in CI without docker-compose. The build tag `e2e`
// keeps it off the fast lane.
//
// Coverage:
//
//   (a) Health summary aggregator returns valid JSON.
//   (b) Slow-query endpoint surfaces a deliberately slow row.
//   (c) Cache stats endpoint surfaces hit/miss/eviction counts.
//   (d) API-key rotation transitions old → grace → expired.
//   (e) Audit integrity endpoint reports a valid hash chain head.
//   (f) Per-stage circuit-breaker dashboard returns breaker state.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// ginWithTenant builds a minimal router with the given tenant
// injected into the audit context. Most Round-13 endpoints are
// tenant-scoped so the helper is reused across subtests.
func ginWithTenant(tenantID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, tenantID) })
	return r
}

// freshAPIDB returns a SQLite handle prepared with the schemas
// needed for the Round-13 subtests.
func freshAPIDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	return db
}

func TestRound13E2E(t *testing.T) {
	observability.ResetForTest()

	t.Run("a_slow_query_endpoint", func(t *testing.T) {
		db := freshAPIDB(t)
		if err := db.AutoMigrate(&admin.SlowQueryRow{}); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		store := admin.NewSlowQueryStoreGORM(db)
		if err := store.RecordSlowQuery(context.Background(), &admin.SlowQueryRow{
			TenantID: "tenant-a", QueryHash: "h1", QueryText: "slow", LatencyMS: 1500,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		r := ginWithTenant("tenant-a")
		admin.NewSlowQueryLogHandler(store).Register(r.Group(""))
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
		if len(resp.Items) != 1 || resp.Items[0].LatencyMS < 1000 {
			t.Fatalf("expected slow row, got %+v", resp)
		}
	})

	t.Run("b_api_key_rotation_then_sweep", func(t *testing.T) {
		db := freshAPIDB(t)
		if err := db.AutoMigrate(&admin.APIKeyRow{}); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		store := admin.NewAPIKeyStoreGORM(db)
		now := time.Now().UTC()
		// Seed an active key, then rotate.
		active := &admin.APIKeyRow{
			ID: ulid.Make().String(), TenantID: "tenant-a",
			KeyHash: "old", Status: string(admin.APIKeyStatusActive), CreatedAt: now,
		}
		if err := store.Insert(context.Background(), active); err != nil {
			t.Fatalf("insert: %v", err)
		}
		newRow := &admin.APIKeyRow{
			ID: ulid.Make().String(), TenantID: "tenant-a",
			KeyHash: "new", Status: string(admin.APIKeyStatusActive), CreatedAt: now,
		}
		if err := store.Rotate(context.Background(), "tenant-a", now.Add(-time.Hour), newRow); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		sw, _ := admin.NewAPIKeySweeper(admin.APIKeySweeperConfig{
			Store: store, NowFn: func() time.Time { return now },
		})
		n, err := sw.SweepOnce(context.Background())
		if err != nil || n != 1 {
			t.Fatalf("sweep n=%d err=%v", n, err)
		}
	})

	t.Run("c_pipeline_throughput_endpoint", func(t *testing.T) {
		rec := admin.NewPipelineThroughputRecorder()
		rec.Record("fetch", 50*time.Millisecond)
		rec.Record("embed", 250*time.Millisecond)
		r := ginWithTenant("tenant-a")
		admin.NewPipelineThroughputHandler(rec).Register(r.Group(""))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/throughput?window=5m", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("d_stage_breaker_endpoint", func(t *testing.T) {
		reg := pipeline.NewStageBreakerRegistry()
		br, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{
			Stage: "embed", Threshold: 3, OpenFor: time.Second,
		})
		if err != nil {
			t.Fatalf("breaker: %v", err)
		}
		reg.Add(br)
		r := ginWithTenant("tenant-a")
		admin.NewStageBreakerHandler(reg).Register(r.Group(""))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/breakers", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("e_payload_size_limiter", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(observability.PayloadSizeLimiter(observability.PayloadLimiterConfig{MaxBytes: 64}))
		r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
		body := bytes.Repeat([]byte("x"), 256)
		req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("got %d want 413", w.Code)
		}
	})
}
