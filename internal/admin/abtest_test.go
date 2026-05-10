package admin_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func TestABTestConfig_Validation(t *testing.T) {
	cases := []*admin.ABTestConfig{
		nil,
		{ExperimentName: "exp", Status: admin.ABTestStatusDraft},
		{TenantID: "t", Status: admin.ABTestStatusDraft},
		{TenantID: "t", ExperimentName: "exp", Status: "invalid"},
		{TenantID: "t", ExperimentName: "exp", Status: admin.ABTestStatusDraft, TrafficSplitPercent: 200},
	}
	for i, tc := range cases {
		if err := tc.Validate(); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
	good := &admin.ABTestConfig{TenantID: "t", ExperimentName: "exp", Status: admin.ABTestStatusActive, TrafficSplitPercent: 50}
	if err := good.Validate(); err != nil {
		t.Fatalf("good case failed: %v", err)
	}
}

func TestABTestRouter_TrafficSplit(t *testing.T) {
	store := admin.NewInMemoryABTestStore()
	if err := store.Upsert(&admin.ABTestConfig{
		TenantID:            "t",
		ExperimentName:      "rerank-v2",
		Status:              admin.ABTestStatusActive,
		TrafficSplitPercent: 50,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	r := admin.NewABTestRouter(store)
	control, variant := 0, 0
	for i := 0; i < 1000; i++ {
		out, err := r.Route("t", "rerank-v2", "user-"+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("route: %v", err)
		}
		if out == nil {
			t.Fatalf("nil outcome on active experiment")
		}
		switch out.Arm {
		case admin.ABTestArmControl:
			control++
		case admin.ABTestArmVariant:
			variant++
		}
	}
	if variant < 400 || variant > 600 {
		t.Fatalf("expected ~500 variant; got %d (control=%d)", variant, control)
	}
}

func TestABTestRouter_DraftIsNoop(t *testing.T) {
	store := admin.NewInMemoryABTestStore()
	_ = store.Upsert(&admin.ABTestConfig{
		TenantID: "t", ExperimentName: "exp", Status: admin.ABTestStatusDraft, TrafficSplitPercent: 50,
	})
	r := admin.NewABTestRouter(store)
	out, _ := r.Route("t", "exp", "user-1")
	if out != nil {
		t.Fatalf("draft must produce nil outcome")
	}
}

func TestABTestRouter_TenantIsolation(t *testing.T) {
	store := admin.NewInMemoryABTestStore()
	_ = store.Upsert(&admin.ABTestConfig{
		TenantID: "tenant-a", ExperimentName: "exp",
		Status: admin.ABTestStatusActive, TrafficSplitPercent: 100,
	})
	r := admin.NewABTestRouter(store)
	out, _ := r.Route("tenant-a", "exp", "u1")
	if out == nil || out.Arm != admin.ABTestArmVariant {
		t.Fatalf("tenant-a should see variant arm; got %+v", out)
	}
	out, _ = r.Route("tenant-b", "exp", "u1")
	if out != nil {
		t.Fatalf("tenant-b should see no experiment")
	}
}

func TestABTestRouter_DeterministicRouting(t *testing.T) {
	store := admin.NewInMemoryABTestStore()
	_ = store.Upsert(&admin.ABTestConfig{
		TenantID: "t", ExperimentName: "exp", Status: admin.ABTestStatusActive, TrafficSplitPercent: 30,
	})
	r := admin.NewABTestRouter(store)
	first, _ := r.Route("t", "exp", "user-42")
	second, _ := r.Route("t", "exp", "user-42")
	if first.Arm != second.Arm {
		t.Fatalf("identical bucket key must produce identical arm")
	}
}

func TestABTestStore_Lifecycle(t *testing.T) {
	s := admin.NewInMemoryABTestStore()
	cfg := &admin.ABTestConfig{TenantID: "t", ExperimentName: "exp", Status: admin.ABTestStatusDraft}
	if err := s.Upsert(cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.Get("t", "exp")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", err, got)
	}
	rows, _ := s.List("t")
	if len(rows) != 1 {
		t.Fatalf("expected 1 row; got %d", len(rows))
	}
	if err := s.Delete("t", "exp"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get("t", "exp"); err == nil {
		t.Fatalf("expected not-found after delete")
	}
}

func TestABTestHandler_UpsertGetListDelete(t *testing.T) {
	store := admin.NewInMemoryABTestStore()
	h, err := admin.NewABTestHandler(store, &fakeAudit{})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)

	body, _ := json.Marshal(admin.ABTestConfig{
		ExperimentName: "rerank-v2", Status: admin.ABTestStatusActive, TrafficSplitPercent: 25,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/retrieval/experiments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("post status=%d body=%s", rr.Code, rr.Body)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/admin/retrieval/experiments", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d", rr.Code)
	}
	var listResp struct {
		Experiments []admin.ABTestConfig `json:"experiments"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &listResp)
	if len(listResp.Experiments) != 1 || listResp.Experiments[0].ExperimentName != "rerank-v2" {
		t.Fatalf("unexpected list: %+v", listResp)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/admin/retrieval/experiments/rerank-v2", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d", rr.Code)
	}
}
