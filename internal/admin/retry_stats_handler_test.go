package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type stubRetryStats struct{ rows []*pipeline.StageRetryStats }

func (s *stubRetryStats) Snapshot() []*pipeline.StageRetryStats { return s.rows }

func TestRetryStatsHandler_Validation(t *testing.T) {
	if _, err := admin.NewRetryStatsHandler(nil); err == nil {
		t.Fatalf("nil source must error")
	}
}

func TestRetryStatsHandler_HTTP(t *testing.T) {
	src := &stubRetryStats{rows: []*pipeline.StageRetryStats{
		{Stage: "fetch", Attempts: 5, Successes: 4, Retries: 1},
	}}
	h, err := admin.NewRetryStatsHandler(src)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/retry-stats", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
	var body struct {
		Stages []pipeline.StageRetryStats `json:"stages"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Stages) != 1 || body.Stages[0].Stage != "fetch" {
		t.Fatalf("unexpected payload: %s", rr.Body)
	}
}
