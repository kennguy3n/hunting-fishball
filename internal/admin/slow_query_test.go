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

func TestSlowQueryHandler_ReturnsOnlySlowRows(t *testing.T) {
	t.Parallel()
	store := admin.NewInMemoryQueryAnalyticsStore()
	ctx := context.Background()
	now := time.Now().UTC()
	// 3 fast rows, 2 slow rows.
	for i := 0; i < 3; i++ {
		_ = store.Record(ctx, &admin.QueryAnalyticsRow{
			TenantID: "t-a", QueryHash: "h", QueryText: "fast", TopK: 5, HitCount: 5,
			LatencyMS: 100, CreatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	_ = store.Record(ctx, &admin.QueryAnalyticsRow{
		TenantID: "t-a", QueryHash: "h", QueryText: "slow-1", TopK: 5, HitCount: 5,
		LatencyMS: 1500, Slow: true, CreatedAt: now,
	})
	_ = store.Record(ctx, &admin.QueryAnalyticsRow{
		TenantID: "t-a", QueryHash: "h", QueryText: "slow-2", TopK: 5, HitCount: 5,
		LatencyMS: 2200, Slow: true, CreatedAt: now.Add(time.Minute),
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	admin.NewSlowQueryHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/queries/slow", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body admin.SlowQueryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("expected 2 slow rows, got %d", len(body.Items))
	}
	for _, it := range body.Items {
		if it.LatencyMS < 1500 {
			t.Errorf("row.latency_ms=%d should be slow", it.LatencyMS)
		}
	}
}

func TestSlowQueryHandler_TenantIsolation(t *testing.T) {
	t.Parallel()
	store := admin.NewInMemoryQueryAnalyticsStore()
	ctx := context.Background()
	_ = store.Record(ctx, &admin.QueryAnalyticsRow{
		TenantID: "t-a", QueryHash: "h", QueryText: "slow",
		LatencyMS: 2000, Slow: true, CreatedAt: time.Now().UTC(),
	})
	_ = store.Record(ctx, &admin.QueryAnalyticsRow{
		TenantID: "t-b", QueryHash: "h", QueryText: "leak",
		LatencyMS: 5000, Slow: true, CreatedAt: time.Now().UTC(),
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	admin.NewSlowQueryHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/queries/slow", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body admin.SlowQueryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Items))
	}
	if body.Items[0].QueryText == "leak" {
		t.Fatalf("tenant-b leaked into tenant-a response")
	}
}

func TestSlowQueryHandler_RequiresTenantContext(t *testing.T) {
	t.Parallel()
	store := admin.NewInMemoryQueryAnalyticsStore()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rg := r.Group("/")
	admin.NewSlowQueryHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/queries/slow", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", w.Code)
	}
}
