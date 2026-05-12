package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLatencyHistogramRecorder_Percentiles(t *testing.T) {
	rec := NewLatencyHistogramRecorder(1000)
	for i := 1; i <= 100; i++ {
		rec.Observe("vector", time.Duration(i)*time.Millisecond)
	}
	rows := rec.SinceMinutes(time.Hour)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	r := rows[0]
	if r.Backend != "vector" || r.Count != 100 {
		t.Fatalf("backend=%q count=%d", r.Backend, r.Count)
	}
	// Nearest-rank percentiles over 1..100: P50=50, P95=95, P99=99.
	if r.P50 != 50 || r.P95 != 95 || r.P99 != 99 {
		t.Fatalf("percentiles wrong p50=%v p95=%v p99=%v", r.P50, r.P95, r.P99)
	}
}

func TestLatencyHistogramRecorder_WindowFilter(t *testing.T) {
	rec := NewLatencyHistogramRecorder(1000)
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	now := time.Now().UTC()
	rec.nowFn = func() time.Time { return t0 }
	rec.Observe("vector", 100*time.Millisecond)
	rec.nowFn = func() time.Time { return now }
	rec.Observe("vector", 200*time.Millisecond)

	rows := rec.SinceMinutes(15 * time.Minute)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Count != 1 || rows[0].P50 != 200 {
		t.Fatalf("expected only the recent sample, got count=%d p50=%v", rows[0].Count, rows[0].P50)
	}
}

func TestLatencyHistogramRecorder_CapacityBounded(t *testing.T) {
	rec := NewLatencyHistogramRecorder(10)
	for i := 0; i < 50; i++ {
		rec.Observe("vector", time.Duration(i)*time.Millisecond)
	}
	rows := rec.SinceMinutes(time.Hour)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Count != 10 {
		t.Fatalf("count=%d want capacity=10", rows[0].Count)
	}
}

func TestLatencyHistogramHandler_GetReturnsBackends(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := NewLatencyHistogramRecorder(100)
	rec.Observe("vector", 10*time.Millisecond)
	rec.Observe("bm25", 5*time.Millisecond)
	rec.Observe("total", 20*time.Millisecond)

	h := NewLatencyHistogramHandler(rec)
	r := gin.New()
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/retrieval/latency-histogram?window_minutes=15", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp LatencyHistogramResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.WindowMinutes != 15 {
		t.Fatalf("window=%d", resp.WindowMinutes)
	}
	if len(resp.Backends) != 3 {
		t.Fatalf("backends=%d", len(resp.Backends))
	}
	want := []string{"bm25", "total", "vector"}
	for i, b := range want {
		if resp.Backends[i].Backend != b {
			t.Fatalf("backends[%d]=%q want %q", i, resp.Backends[i].Backend, b)
		}
	}
}

func TestLatencyHistogramHandler_NilRecorderReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewLatencyHistogramHandler(nil)
	r := gin.New()
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/retrieval/latency-histogram", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}
