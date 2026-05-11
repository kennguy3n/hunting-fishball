package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func TestIsolationAuditor_Validation(t *testing.T) {
	if _, err := admin.NewIsolationAuditor(nil); err == nil {
		t.Fatalf("nil checkers must error")
	}
}

func TestIsolationAuditor_AllPass(t *testing.T) {
	checkers := []admin.IsolationChecker{
		admin.CheckerFunc{
			BackendName: "qdrant",
			Fn: func(_ context.Context) admin.IsolationCheckResult {
				return admin.IsolationCheckResult{Pass: true, Inspected: 10}
			},
		},
		admin.CheckerFunc{
			BackendName: "redis",
			Fn: func(_ context.Context) admin.IsolationCheckResult {
				return admin.IsolationCheckResult{Pass: true, Inspected: 5}
			},
		},
	}
	a, err := admin.NewIsolationAuditor(checkers)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	rep := a.Run(context.Background())
	if !rep.OverallPass {
		t.Fatalf("expected overall pass; got %+v", rep)
	}
	if len(rep.Results) != 2 {
		t.Fatalf("expected 2 results; got %d", len(rep.Results))
	}
}

func TestIsolationAuditor_FailingBackendFlipsOverall(t *testing.T) {
	a, _ := admin.NewIsolationAuditor([]admin.IsolationChecker{
		admin.CheckerFunc{BackendName: "qdrant", Fn: func(_ context.Context) admin.IsolationCheckResult {
			return admin.IsolationCheckResult{Pass: true}
		}},
		admin.CheckerFunc{BackendName: "redis", Fn: func(_ context.Context) admin.IsolationCheckResult {
			return admin.IsolationCheckResult{
				Pass:     false,
				Findings: []string{"key 'foo' missing tenant prefix"},
			}
		}},
	})
	rep := a.Run(context.Background())
	if rep.OverallPass {
		t.Fatalf("a single failing backend must flip overall_pass")
	}
	var found bool
	for _, r := range rep.Results {
		if r.Backend == "redis" {
			if r.Pass {
				t.Fatalf("redis should be marked failed")
			}
			if len(r.Findings) == 0 {
				t.Fatalf("redis findings missing")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("redis backend missing from report")
	}
}

func TestIsolationHandler_HTTP(t *testing.T) {
	a, _ := admin.NewIsolationAuditor([]admin.IsolationChecker{
		admin.CheckerFunc{BackendName: "qdrant", Fn: func(_ context.Context) admin.IsolationCheckResult {
			return admin.IsolationCheckResult{Pass: true}
		}},
	})
	h, err := admin.NewIsolationHandler(a)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/isolation-check", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
	var rep admin.IsolationReport
	_ = json.Unmarshal(rr.Body.Bytes(), &rep)
	if !rep.OverallPass {
		t.Fatalf("expected pass; got %+v", rep)
	}
}
