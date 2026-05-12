package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestPipelineThroughputRecorder_BucketsAndAvgLatency(t *testing.T) {
	rec := NewPipelineThroughputRecorder()
	now := time.Now().UTC().Truncate(time.Minute)
	rec.nowFn = func() time.Time { return now }

	rec.Record("fetch", 100*time.Millisecond)
	rec.Record("fetch", 200*time.Millisecond)
	rec.Record("parse", 50*time.Millisecond)

	rows := rec.Window(5 * time.Minute)
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	byStage := map[string]PipelineThroughputRow{}
	for _, r := range rows {
		byStage[r.Stage] = r
	}
	if got := byStage["fetch"]; got.Count != 2 || got.AvgLatency != 150 {
		t.Fatalf("fetch=%+v", got)
	}
	if got := byStage["parse"]; got.Count != 1 || got.AvgLatency != 50 {
		t.Fatalf("parse=%+v", got)
	}
}

func TestPipelineThroughputRecorder_DropsOldBuckets(t *testing.T) {
	rec := NewPipelineThroughputRecorder()
	t0 := time.Now().UTC().Truncate(time.Minute)
	rec.nowFn = func() time.Time { return t0 }
	rec.Record("fetch", 0)

	// Jump forward past the retention horizon (60 buckets).
	rec.nowFn = func() time.Time { return t0.Add(90 * time.Minute) }
	rec.Record("fetch", 10*time.Millisecond)

	rows := rec.Window(time.Hour)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Count != 1 {
		t.Fatalf("count=%d want 1 (old bucket should have been dropped)", rows[0].Count)
	}
}

func TestPipelineThroughputHandler_Get(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := NewPipelineThroughputRecorder()
	rec.Record("fetch", 100*time.Millisecond)
	rec.Record("embed", 200*time.Millisecond)

	h := NewPipelineThroughputHandler(rec)
	r := gin.New()
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/throughput?window=10m", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp PipelineThroughputResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Window != "10m0s" {
		t.Fatalf("window=%q", resp.Window)
	}
	if len(resp.Stages) != 2 {
		t.Fatalf("stages=%d", len(resp.Stages))
	}
}

func TestPipelineThroughputHandler_NilRecorder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewPipelineThroughputHandler(nil)
	r := gin.New()
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/throughput", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}
