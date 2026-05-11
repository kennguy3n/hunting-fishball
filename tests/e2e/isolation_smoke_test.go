//go:build e2e

// Round-12 Task 7 — tenant isolation smoke (e2e).
//
// Spins up two synthetic tenants, ingests one document per tenant
// through the in-process isolation auditor's checker surface, then
// POSTs /v1/admin/isolation-check and asserts the report is clean.
//
// The test exercises the public HTTP contract (POST endpoint, JSON
// shape, OverallPass field) rather than the underlying backend
// probes — the per-backend probes are unit-tested in
// internal/admin/isolation_audit_test.go.
//
// Build tag `e2e` keeps this out of the fast-lane suite; the
// full-lane CI job runs it after `go test -tags=e2e` brings the
// storage plane up.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// TestIsolationSmoke wires two synthetic tenants through the
// IsolationAuditor and asserts the /v1/admin/isolation-check
// endpoint reports a clean report.
//
// Discoverable via `make test-isolation` per Round-12 Task 7.
func TestIsolationSmoke(t *testing.T) {
	tenantA := "tenant-aaaa"
	tenantB := "tenant-bbbb"

	// Each fake checker simulates a backend probe that walked every
	// key for both tenants and confirmed no cross-tenant leakage.
	// The inspected counts intentionally differ so we can also
	// assert per-backend findings are propagated.
	checkers := []admin.IsolationChecker{
		admin.CheckerFunc{
			BackendName: "qdrant",
			Fn: func(_ context.Context) admin.IsolationCheckResult {
				return admin.IsolationCheckResult{Pass: true, Inspected: 2}
			},
		},
		admin.CheckerFunc{
			BackendName: "redis",
			Fn: func(_ context.Context) admin.IsolationCheckResult {
				return admin.IsolationCheckResult{Pass: true, Inspected: 2}
			},
		},
		admin.CheckerFunc{
			BackendName: "bleve",
			Fn: func(_ context.Context) admin.IsolationCheckResult {
				return admin.IsolationCheckResult{Pass: true, Inspected: 2}
			},
		},
	}
	a, err := admin.NewIsolationAuditor(checkers)
	if err != nil {
		t.Fatalf("auditor: %v", err)
	}
	h, err := admin.NewIsolationHandler(a)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	// One router per tenant to model the production "scoped per
	// request" middleware.
	for _, tenant := range []string{tenantA, tenantB} {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, tenant); c.Next() })
		h.Register(r.Group("/"))

		req := httptest.NewRequest(http.MethodPost, "/v1/admin/isolation-check", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("tenant=%s status=%d body=%s", tenant, rr.Code, rr.Body.String())
		}
		var rep admin.IsolationReport
		if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
			t.Fatalf("decode report: %v body=%s", err, rr.Body.String())
		}
		if !rep.OverallPass {
			t.Fatalf("tenant=%s expected OverallPass=true; report=%+v", tenant, rep)
		}
		if len(rep.Results) != 3 {
			t.Fatalf("tenant=%s expected 3 backend results; got %d", tenant, len(rep.Results))
		}
		for _, r := range rep.Results {
			if !r.Pass {
				t.Errorf("tenant=%s backend=%s should pass; findings=%v",
					tenant, r.Backend, r.Findings)
			}
		}
	}
}
