package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func TestCacheStatsHandler_ComputesHitRate(t *testing.T) {
	t.Parallel()
	store := admin.NewInMemoryQueryAnalyticsStore()
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		_ = store.Record(ctx, &admin.QueryAnalyticsRow{
			TenantID: "t-a", QueryHash: "h", QueryText: "q",
			LatencyMS: 50, CacheHit: true, CreatedAt: now.Add(-time.Minute),
		})
	}
	for i := 0; i < 7; i++ {
		_ = store.Record(ctx, &admin.QueryAnalyticsRow{
			TenantID: "t-a", QueryHash: "h", QueryText: "q",
			LatencyMS: 50, CacheHit: false, CreatedAt: now.Add(-time.Minute),
		})
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	admin.NewCacheStatsHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/cache-stats", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body admin.CacheStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Hits != 3 {
		t.Errorf("hits=%d want=3", body.Hits)
	}
	if body.Misses != 7 {
		t.Errorf("misses=%d want=7", body.Misses)
	}
	if body.Total != 10 {
		t.Errorf("total=%d want=10", body.Total)
	}
	if body.HitRatePct < 29.99 || body.HitRatePct > 30.01 {
		t.Errorf("hit_rate_pct=%.4f want≈30", body.HitRatePct)
	}
}

func TestCacheStatsHandler_TenantIsolation(t *testing.T) {
	t.Parallel()
	store := admin.NewInMemoryQueryAnalyticsStore()
	ctx := context.Background()
	now := time.Now().UTC()
	_ = store.Record(ctx, &admin.QueryAnalyticsRow{
		TenantID: "t-a", QueryText: "q", LatencyMS: 1, CacheHit: true, CreatedAt: now,
	})
	for i := 0; i < 5; i++ {
		_ = store.Record(ctx, &admin.QueryAnalyticsRow{
			TenantID: "t-b", QueryText: "q", LatencyMS: 1, CacheHit: false, CreatedAt: now,
		})
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	admin.NewCacheStatsHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/cache-stats", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body admin.CacheStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 {
		t.Fatalf("total=%d leaked tenant-b data", body.Total)
	}
}

func TestCacheStatsHandler_EmptyWindowReturnsZero(t *testing.T) {
	t.Parallel()
	store := admin.NewInMemoryQueryAnalyticsStore()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	admin.NewCacheStatsHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/cache-stats", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body admin.CacheStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 0 || body.HitRatePct != 0 {
		t.Fatalf("expected zero totals; got %+v", body)
	}
}
