package observability_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

func TestMetricsHandler_ExposesRegisteredCollectors(t *testing.T) {
	t.Parallel()
	// Pre-populate one observation so the histograms / counters
	// have non-zero output the scraper can verify.
	observability.ObserveStageDuration("fetch", 0.1)
	observability.ObserveBackendDuration("vector", 0.05)
	observability.SetBackendHits("vector", 7)
	observability.SetKafkaConsumerLag("ingest", "0", "context-engine", 42)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	observability.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"context_engine_pipeline_stage_duration_seconds",
		"context_engine_retrieval_backend_duration_seconds",
		"context_engine_retrieval_backend_hits",
		"context_engine_kafka_consumer_lag",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing collector %q in /metrics output", want)
		}
	}
}

func TestPrometheusMiddleware_RecordsRequestCount(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.PrometheusMiddleware())
	r.GET("/echo", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	srv := httptest.NewServer(r)
	defer srv.Close()
	for i := 0; i < 3; i++ {
		resp, err := srv.Client().Get(srv.URL + "/echo")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_ = resp.Body.Close()
	}
	rr := httptest.NewRecorder()
	observability.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `context_engine_api_requests_total{method="GET",path="/echo",status="200"}`) {
		t.Errorf("counter not recorded; body excerpt:\n%s", excerpt(body, "context_engine_api_requests_total"))
	}
	if !strings.Contains(body, `context_engine_api_request_duration_seconds_count{method="GET",path="/echo"}`) {
		t.Errorf("histogram not recorded; body excerpt:\n%s", excerpt(body, "context_engine_api_request_duration_seconds_count"))
	}
}

// excerpt returns a few lines around the first match for needle in
// body, for friendlier failure output.
func excerpt(body, needle string) string {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return "(no match)"
	}
	start := idx - 80
	if start < 0 {
		start = 0
	}
	end := idx + 200
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}
