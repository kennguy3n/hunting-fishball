package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func TestCacheWarmHandler_ExplicitTuples(t *testing.T) {
	var called int
	exec := admin.CacheWarmExecutorFunc(func(_ context.Context, tuples []admin.CacheWarmTuple) admin.CacheWarmSummary {
		called++
		summary := admin.CacheWarmSummary{Total: len(tuples), Succeeded: len(tuples)}
		for _, tup := range tuples {
			summary.Results = append(summary.Results, admin.CacheWarmResult{Query: tup.Query, Hits: 3})
		}
		return summary
	})
	h, err := admin.NewCacheWarmHandler(admin.CacheWarmHandlerConfig{Warmer: exec})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	body := `{"tuples":[{"query":"hello"},{"query":"world"}]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/retrieval/warm-cache", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d body=%s", w.Code, w.Body.String())
	}
	if called != 1 {
		t.Fatalf("expected exec called once; got %d", called)
	}
	var summary admin.CacheWarmSummary
	_ = json.Unmarshal(w.Body.Bytes(), &summary)
	if summary.Total != 2 {
		t.Fatalf("expected 2 tuples warmed; got %d", summary.Total)
	}
}

func TestCacheWarmHandler_AutoFromAnalytics(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	rec, _ := admin.NewQueryAnalyticsRecorder(store)
	for i := 0; i < 3; i++ {
		rec.Record(context.Background(), admin.QueryAnalyticsEvent{TenantID: "ta", QueryText: "popular"})
	}
	rec.Record(context.Background(), admin.QueryAnalyticsEvent{TenantID: "ta", QueryText: "uncommon"})
	var seen []admin.CacheWarmTuple
	exec := admin.CacheWarmExecutorFunc(func(_ context.Context, tuples []admin.CacheWarmTuple) admin.CacheWarmSummary {
		seen = tuples
		return admin.CacheWarmSummary{Total: len(tuples), Succeeded: len(tuples)}
	})
	h, _ := admin.NewCacheWarmHandler(admin.CacheWarmHandlerConfig{Warmer: exec, Analytics: store, AutoTopN: 5})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/retrieval/warm-cache?auto=true", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 tuples (popular + uncommon); got %d", len(seen))
	}
	if seen[0].Query != "popular" {
		t.Fatalf("expected most-frequent first; got %s", seen[0].Query)
	}
}

func TestCacheWarmHandler_TenantIsolation(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	rec, _ := admin.NewQueryAnalyticsRecorder(store)
	rec.Record(context.Background(), admin.QueryAnalyticsEvent{TenantID: "tb", QueryText: "other tenant"})
	var seen []admin.CacheWarmTuple
	exec := admin.CacheWarmExecutorFunc(func(_ context.Context, tuples []admin.CacheWarmTuple) admin.CacheWarmSummary {
		seen = tuples
		return admin.CacheWarmSummary{}
	})
	h, _ := admin.NewCacheWarmHandler(admin.CacheWarmHandlerConfig{Warmer: exec, Analytics: store, AutoTopN: 5})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/retrieval/warm-cache?auto=true", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest && len(seen) != 0 {
		t.Fatalf("expected no tuples warmed for ta; got %d", len(seen))
	}
}
