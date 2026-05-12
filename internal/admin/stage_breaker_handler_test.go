package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type fakeInspector struct {
	rows []pipeline.StageBreakerSnapshot
}

func (f *fakeInspector) Snapshot() []pipeline.StageBreakerSnapshot { return f.rows }

func TestStageBreakerHandler_ListSortsByStage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	insp := &fakeInspector{rows: []pipeline.StageBreakerSnapshot{
		{Stage: "store", State: "closed"},
		{Stage: "fetch", State: "open", FailCount: 5, Threshold: 3},
		{Stage: "parse", State: "half-open", ProbeInFlight: true, Threshold: 3},
	}}
	h := admin.NewStageBreakerHandler(insp)
	r := gin.New()
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/breakers", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp admin.StageBreakerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items=%d want 3", len(resp.Items))
	}
	want := []string{"fetch", "parse", "store"}
	for i, s := range want {
		if resp.Items[i].Stage != s {
			t.Fatalf("items[%d].stage=%q want %q", i, resp.Items[i].Stage, s)
		}
	}
	if resp.Items[0].State != "open" || resp.Items[0].FailCount != 5 {
		t.Fatalf("fetch row mismatch: %+v", resp.Items[0])
	}
	if !resp.Items[1].ProbeInFlight {
		t.Fatalf("parse row should mark probe_in_flight")
	}
}

func TestStageBreakerHandler_NilInspectorReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := admin.NewStageBreakerHandler(nil)
	r := gin.New()
	h.Register(r.Group(""))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/pipeline/breakers", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp admin.StageBreakerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("items=%v want 0", resp.Items)
	}
}

func TestStageBreakerRegistry_Snapshot(t *testing.T) {
	reg := pipeline.NewStageBreakerRegistry()
	parse, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{
		Stage:     "parse",
		Threshold: 2,
	})
	if err != nil {
		t.Fatalf("new breaker: %v", err)
	}
	embed, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{
		Stage:     "embed",
		Threshold: 3,
	})
	if err != nil {
		t.Fatalf("new breaker: %v", err)
	}
	reg.Add(parse)
	reg.Add(embed)

	parse.OnFailure()
	parse.OnFailure() // crosses threshold → opens
	embed.OnFailure()

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snap=%d want 2", len(snap))
	}
	byStage := map[string]pipeline.StageBreakerSnapshot{}
	for _, s := range snap {
		byStage[s.Stage] = s
	}
	if got := byStage["parse"]; got.State != "open" {
		t.Fatalf("parse state=%q want open", got.State)
	}
	if got := byStage["embed"]; got.State != "closed" || got.FailCount != 1 {
		t.Fatalf("embed state=%q fail=%d", got.State, got.FailCount)
	}
}
