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

func init() { gin.SetMode(gin.TestMode) }

func TestQueryAnalytics_RecordAndList(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	rec, err := admin.NewQueryAnalyticsRecorder(store)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	now := time.Now().UTC()
	rec.Record(context.Background(), admin.QueryAnalyticsEvent{
		TenantID:       "ta",
		QueryText:      "hello world",
		TopK:           5,
		HitCount:       3,
		CacheHit:       false,
		LatencyMS:      42,
		BackendTimings: map[string]int64{"vector": 10, "bm25": 12},
		At:             now,
	})
	rec.Record(context.Background(), admin.QueryAnalyticsEvent{
		TenantID:  "ta",
		QueryText: "hello world",
		HitCount:  4,
		LatencyMS: 30,
		CacheHit:  true,
		At:        now.Add(time.Second),
	})
	rec.Record(context.Background(), admin.QueryAnalyticsEvent{
		TenantID:  "tb",
		QueryText: "other tenant",
		HitCount:  1,
		LatencyMS: 50,
		At:        now,
	})
	rows, err := store.List(context.Background(), admin.QueryAnalyticsQuery{TenantID: "ta", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows for ta; got %d", len(rows))
	}
	if rows[0].LatencyMS != 30 || !rows[0].CacheHit {
		t.Fatalf("expected most-recent first; got %+v", rows[0])
	}
	// tenant isolation
	rowsB, _ := store.List(context.Background(), admin.QueryAnalyticsQuery{TenantID: "tb"})
	if len(rowsB) != 1 {
		t.Fatalf("expected 1 row for tb; got %d", len(rowsB))
	}
}

func TestQueryAnalytics_TopQueries(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	rec, _ := admin.NewQueryAnalyticsRecorder(store)
	for i := 0; i < 5; i++ {
		rec.Record(context.Background(), admin.QueryAnalyticsEvent{
			TenantID: "ta", QueryText: "frequent", LatencyMS: 100, HitCount: 5, CacheHit: i%2 == 0,
		})
	}
	for i := 0; i < 2; i++ {
		rec.Record(context.Background(), admin.QueryAnalyticsEvent{
			TenantID: "ta", QueryText: "rare", LatencyMS: 200, HitCount: 1,
		})
	}
	top, err := store.TopQueries(context.Background(), admin.QueryAnalyticsQuery{TenantID: "ta", Limit: 5})
	if err != nil {
		t.Fatalf("top: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("expected 2 top queries; got %d", len(top))
	}
	if top[0].Count != 5 {
		t.Fatalf("expected frequent first with count=5; got %+v", top[0])
	}
	if top[0].AvgLatencyMS != 100 {
		t.Fatalf("avg latency wrong: %v", top[0].AvgLatencyMS)
	}
	if top[0].CacheHitPct < 50 || top[0].CacheHitPct > 70 {
		t.Fatalf("cache hit pct out of range: %v", top[0].CacheHitPct)
	}
}

func TestQueryAnalytics_Handler_ListAndTop(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	rec, _ := admin.NewQueryAnalyticsRecorder(store)
	for i := 0; i < 3; i++ {
		rec.Record(context.Background(), admin.QueryAnalyticsEvent{
			TenantID: "ta", QueryText: "q", LatencyMS: 10, HitCount: 1,
		})
	}
	h, err := admin.NewQueryAnalyticsHandler(store)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "ta")
		c.Next()
	})
	h.Register(r.Group("/"))
	// list
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/queries?limit=2", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status: %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Events []*admin.QueryAnalyticsRow `json:"events"`
		Count  int                        `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Count != 2 {
		t.Fatalf("limit not applied: %d", body.Count)
	}
	// top
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/queries/top", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("top status: %d", w.Code)
	}
	var topBody struct {
		Top   []admin.TopQuery `json:"top"`
		Count int              `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &topBody)
	if topBody.Count != 1 || topBody.Top[0].Count != 3 {
		t.Fatalf("unexpected top: %+v", topBody)
	}
}

func TestQueryAnalytics_Handler_MissingTenant(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	h, _ := admin.NewQueryAnalyticsHandler(store)
	r := gin.New()
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/queries", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401; got %d", w.Code)
	}
}

func TestQueryAnalytics_Handler_InvalidTime(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	h, _ := admin.NewQueryAnalyticsHandler(store)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "ta")
		c.Next()
	})
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/queries?since=not-a-date", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", w.Code)
	}
}
