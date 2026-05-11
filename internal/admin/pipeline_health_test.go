package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func TestPipelineHealthAggregator_ReadsStageMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	hist := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "context_engine_pipeline_stage_duration_seconds",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0},
	}, []string{"stage"})
	depth := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "context_engine_pipeline_channel_depth"}, []string{"stage"})
	retries := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "context_engine_pipeline_retries_total"}, []string{"stage"})
	reg.MustRegister(hist, depth, retries)
	for i := 0; i < 10; i++ {
		hist.WithLabelValues("fetch").Observe(0.02)
	}
	depth.WithLabelValues("fetch").Set(7)
	retries.WithLabelValues("fetch").Add(3)

	agg, err := admin.NewPipelineHealthAggregator(reg)
	if err != nil {
		t.Fatalf("agg: %v", err)
	}
	rep, err := agg.Aggregate()
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var fetch admin.PipelineStageHealth
	for _, s := range rep.Stages {
		if s.Stage == "fetch" {
			fetch = s
			break
		}
	}
	if fetch.Throughput != 10 || fetch.QueueDepth != 7 || fetch.Retries != 3 {
		t.Fatalf("fetch wrong: %+v", fetch)
	}
}

func TestPipelineHealthHandler_RequiresTenant(t *testing.T) {
	reg := prometheus.NewRegistry()
	agg, _ := admin.NewPipelineHealthAggregator(reg)
	h, _ := admin.NewPipelineHealthHandler(agg)
	r := gin.New()
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401; got %d", w.Code)
	}
}

func TestPipelineHealthHandler_Returns200(t *testing.T) {
	reg := prometheus.NewRegistry()
	agg, _ := admin.NewPipelineHealthAggregator(reg)
	h, _ := admin.NewPipelineHealthHandler(agg)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d", w.Code)
	}
	var rep admin.PipelineHealthReport
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if len(rep.Stages) != 4 {
		t.Fatalf("expected 4 stages; got %d", len(rep.Stages))
	}
}
