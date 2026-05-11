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

// noTime is the zero time used to disable since/until filters in
// aggregator tests.
var noTime = time.Time{}

func seedAnalytics(t *testing.T, store *admin.InMemoryQueryAnalyticsStore) {
	t.Helper()
	rec, _ := admin.NewQueryAnalyticsRecorder(store)
	// control: 5 hits @ 100ms, 2 cache hits
	for i := 0; i < 5; i++ {
		rec.Record(context.Background(), admin.QueryAnalyticsEvent{
			TenantID: "ta", QueryText: "q", LatencyMS: 100, HitCount: 5,
			ExperimentName: "exp1", ExperimentArm: "control",
			CacheHit: i < 2,
		})
	}
	// variant: 5 hits @ 50ms, 5 cache hits
	for i := 0; i < 5; i++ {
		rec.Record(context.Background(), admin.QueryAnalyticsEvent{
			TenantID: "ta", QueryText: "q", LatencyMS: 50, HitCount: 7,
			ExperimentName: "exp1", ExperimentArm: "variant",
			CacheHit: true,
		})
	}
	// noise: different experiment
	rec.Record(context.Background(), admin.QueryAnalyticsEvent{
		TenantID: "ta", QueryText: "x", LatencyMS: 999,
		ExperimentName: "other", ExperimentArm: "control",
	})
}

func TestABTestResults_Aggregate(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	seedAnalytics(t, store)
	agg, err := admin.NewABTestResultsAggregator(store)
	if err != nil {
		t.Fatalf("agg: %v", err)
	}
	resp, err := agg.Aggregate(context.Background(), "ta", "exp1", noTime, noTime)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(resp.Arms) != 2 {
		t.Fatalf("expected 2 arms; got %d", len(resp.Arms))
	}
	if resp.Arms[0].Arm != "control" {
		t.Fatalf("expected control first; got %s", resp.Arms[0].Arm)
	}
	if resp.Arms[0].Count != 5 || resp.Arms[0].AvgLatencyMS != 100 {
		t.Fatalf("control aggregate wrong: %+v", resp.Arms[0])
	}
	if resp.Arms[0].CacheHitPercent != 40 {
		t.Fatalf("expected 40%% cache hit; got %v", resp.Arms[0].CacheHitPercent)
	}
	if resp.Arms[1].Arm != "variant" || resp.Arms[1].AvgLatencyMS != 50 {
		t.Fatalf("variant wrong: %+v", resp.Arms[1])
	}
	if resp.Arms[1].CacheHitPercent != 100 {
		t.Fatalf("variant cache hit wrong: %v", resp.Arms[1].CacheHitPercent)
	}
}

func TestABTestResults_TenantIsolation(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	seedAnalytics(t, store)
	agg, _ := admin.NewABTestResultsAggregator(store)
	resp, err := agg.Aggregate(context.Background(), "tb", "exp1", noTime, noTime)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(resp.Arms) != 0 {
		t.Fatalf("expected 0 arms for other tenant; got %d", len(resp.Arms))
	}
}

func TestABTestResults_Handler(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	seedAnalytics(t, store)
	agg, _ := admin.NewABTestResultsAggregator(store)
	h, err := admin.NewABTestResultsHandler(agg)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "ta")
		c.Next()
	})
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/retrieval/experiments/exp1/results", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d body=%s", w.Code, w.Body.String())
	}
	var body admin.ABTestResultsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Arms) != 2 {
		t.Fatalf("expected 2 arms; got %d", len(body.Arms))
	}
}
