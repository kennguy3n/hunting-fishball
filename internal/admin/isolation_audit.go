package admin

// isolation_audit.go — Round-6 Task 17.
//
// Cross-tenant data-isolation audit. The handler asks every
// configured backend to verify it is partitioning by tenant_id and
// returns a structured report:
//
//   POST /v1/admin/isolation-check
//
// Operators run this on a schedule (or after a migration) to
// catch backends that have drifted off the multi-tenant invariants
// before they leak data across tenants.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// IsolationCheckResult is the per-backend pass/fail entry.
type IsolationCheckResult struct {
	Backend   string         `json:"backend"`
	Pass      bool           `json:"pass"`
	Findings  []string       `json:"findings,omitempty"`
	Inspected int            `json:"inspected"`
	Details   map[string]any `json:"details,omitempty"`
}

// IsolationReport bundles the per-backend results.
type IsolationReport struct {
	GeneratedAt time.Time              `json:"generated_at"`
	OverallPass bool                   `json:"overall_pass"`
	Results     []IsolationCheckResult `json:"results"`
}

// IsolationChecker is one backend probe.
type IsolationChecker interface {
	Name() string
	Check(ctx context.Context) IsolationCheckResult
}

// CheckerFunc adapts an inline closure to the IsolationChecker
// interface.
type CheckerFunc struct {
	BackendName string
	Fn          func(ctx context.Context) IsolationCheckResult
}

// Name implements IsolationChecker.
func (c CheckerFunc) Name() string { return c.BackendName }

// Check implements IsolationChecker.
func (c CheckerFunc) Check(ctx context.Context) IsolationCheckResult {
	if c.Fn == nil {
		return IsolationCheckResult{Backend: c.BackendName, Pass: true}
	}
	res := c.Fn(ctx)
	if res.Backend == "" {
		res.Backend = c.BackendName
	}
	return res
}

// IsolationAuditor runs every registered IsolationChecker in order
// and aggregates the results into a structured report.
type IsolationAuditor struct {
	checkers []IsolationChecker
	now      func() time.Time
}

// NewIsolationAuditor validates and constructs the auditor.
func NewIsolationAuditor(checkers []IsolationChecker) (*IsolationAuditor, error) {
	if len(checkers) == 0 {
		return nil, errors.New("isolation: no checkers configured")
	}
	return &IsolationAuditor{checkers: checkers, now: time.Now}, nil
}

// Run executes every checker and returns a structured report.
func (a *IsolationAuditor) Run(ctx context.Context) IsolationReport {
	rep := IsolationReport{GeneratedAt: a.now().UTC(), OverallPass: true}
	for _, c := range a.checkers {
		res := c.Check(ctx)
		if res.Backend == "" {
			res.Backend = c.Name()
		}
		if !res.Pass {
			rep.OverallPass = false
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

// IsolationHandler exposes the admin HTTP surface.
type IsolationHandler struct {
	auditor *IsolationAuditor
}

// NewIsolationHandler validates and constructs the handler.
func NewIsolationHandler(auditor *IsolationAuditor) (*IsolationHandler, error) {
	if auditor == nil {
		return nil, errors.New("isolation handler: nil auditor")
	}
	return &IsolationHandler{auditor: auditor}, nil
}

// Register attaches the route.
func (h *IsolationHandler) Register(g *gin.RouterGroup) {
	g.POST("/v1/admin/isolation-check", h.run)
}

func (h *IsolationHandler) run(c *gin.Context) {
	rep := h.auditor.Run(c.Request.Context())
	c.JSON(http.StatusOK, rep)
}
