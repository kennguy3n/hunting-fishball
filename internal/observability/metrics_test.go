package observability_test

import (
	"net/http"
	"net/http/httptest"
	"os"
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

// TestMetrics_NoTenantIDLabel — Round-11 Task 12.
//
// Grep-style assertion against the metrics.go source: no
// Prometheus metric registration may list "tenant_id" as a
// label. Tenant_id is unbounded (each new tenant is a new
// label value) so it must live in structured logs only — the
// cardinality policy is documented at the top of metrics.go.
//
// The test scans the actual source file rather than the live
// Registry because Prometheus does not expose label names off
// a generic Collector.
func TestMetrics_NoTenantIDLabel(t *testing.T) {
	t.Parallel()
	const path = "metrics.go"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)
	// Look for any []string{...} or single-quoted "tenant_id"
	// adjacent to a NewCounter*/NewGauge*/NewHistogram*/NewSummary*
	// constructor. Cheap approximation: any line containing
	// "\"tenant_id\"" must be inside a comment.
	for i, line := range splitLines(src) {
		if !strings.Contains(line, "\"tenant_id\"") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		// Allow slog.Warn(..., "tenant_id", ...) — that's a log
		// field, not a Prometheus label. The cheap check is that
		// `slog.Warn` or `Logger` appears on the same line.
		if strings.Contains(line, "slog.") || strings.Contains(line, "Logger") {
			continue
		}
		t.Fatalf("metrics.go:%d uses tenant_id outside slog/comment, which violates the cardinality policy:\n\t%s", i+1, trimmed)
	}
}

func splitLines(s string) []string {
	out := []string{}
	cur := strings.Builder{}
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
