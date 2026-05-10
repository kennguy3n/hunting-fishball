package eval

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// Retrieve is the production retrieval call signature exposed by
// the retrieval.Handler. We declare it locally so the eval package
// stays a leaf — cmd/api adapts handler.RetrieveWithSnapshot (and
// the live policy resolver) into this shape via a closure when
// wiring HandlerConfig.Retrieve.
type RetrieveFunc func(ctx context.Context, req RetrieveRequest) ([]RetrieveHit, error)

// HandlerConfig wires the eval admin endpoint.
type HandlerConfig struct {
	// Suites loads suites by (tenant_id, name). Required.
	Suites SuiteRepository

	// Retrieve runs a single retrieval against the live stack.
	// Required — the runner cannot score without it.
	Retrieve RetrieveFunc

	// Audit records eval.run events. Optional but strongly
	// recommended in production so operators can correlate the
	// suite run with downstream policy/regression alerts.
	Audit *audit.Repository
}

// Handler exposes the eval admin endpoint.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler validates the config and returns a ready Handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Suites == nil {
		return nil, errors.New("eval: HandlerConfig.Suites is required")
	}
	if cfg.Retrieve == nil {
		return nil, errors.New("eval: HandlerConfig.Retrieve is required")
	}
	return &Handler{cfg: cfg}, nil
}

// Register mounts the admin endpoints on rg:
//
//	POST /v1/admin/eval/suites           — create or update a suite
//	GET  /v1/admin/eval/suites           — list suites for the tenant
//	GET  /v1/admin/eval/suites/:name     — fetch a single suite
//	POST /v1/admin/eval/run              — run a suite (by name OR
//	                                       inline body) and return
//	                                       the EvalReport
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/eval/suites", h.upsertSuite)
	rg.GET("/v1/admin/eval/suites", h.listSuites)
	rg.GET("/v1/admin/eval/suites/:name", h.getSuite)
	rg.POST("/v1/admin/eval/run", h.run)
}

// runRequest is either {"suite_name": "..."} (server-side suite
// reference) or {"suite": {...}} (inline suite for ad-hoc runs).
type runRequest struct {
	SuiteName string     `json:"suite_name,omitempty"`
	Suite     *EvalSuite `json:"suite,omitempty"`
}

func (h *Handler) run(c *gin.Context) {
	tenantID, ok := tenantFromCtx(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}

	var body runRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	suite, err := h.resolveSuite(c.Request.Context(), tenantID, body)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrSuiteNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	suite.TenantID = tenantID
	if err := suite.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	report, err := Run(c.Request.Context(), retrieveAdapter{fn: h.cfg.Retrieve}, suite)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if h.cfg.Audit != nil {
		actor, _ := c.Get(audit.ActorContextKey)
		actorID, _ := actor.(string)
		_ = h.cfg.Audit.Create(c.Request.Context(), audit.NewAuditLog(
			tenantID, actorID, audit.Action("eval.run"),
			"eval_suite", suite.Name,
			audit.JSONMap{
				"k":            report.K,
				"cases":        len(report.Cases),
				"failed":       report.FailedCases,
				"precision_at": report.Aggregate.PrecisionAtK,
				"recall_at":    report.Aggregate.RecallAtK,
				"mrr":          report.Aggregate.MRR,
				"ndcg":         report.Aggregate.NDCG,
				"duration_ms":  report.Duration.Milliseconds(),
			}, ""))
	}

	c.JSON(http.StatusOK, report)
}

func (h *Handler) resolveSuite(ctx context.Context, tenantID string, body runRequest) (EvalSuite, error) {
	if body.Suite != nil {
		return *body.Suite, nil
	}
	if body.SuiteName == "" {
		return EvalSuite{}, errors.New("eval: run requires suite_name or suite")
	}
	return h.cfg.Suites.Get(ctx, tenantID, body.SuiteName)
}

func (h *Handler) upsertSuite(c *gin.Context) {
	tenantID, ok := tenantFromCtx(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var suite EvalSuite
	if err := c.ShouldBindJSON(&suite); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	suite.TenantID = tenantID
	if err := suite.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.cfg.Suites.Save(c.Request.Context(), suite); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, suite)
}

func (h *Handler) getSuite(c *gin.Context) {
	tenantID, ok := tenantFromCtx(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	name := c.Param("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing suite name"})
		return
	}
	suite, err := h.cfg.Suites.Get(c.Request.Context(), tenantID, name)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrSuiteNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, suite)
}

func (h *Handler) listSuites(c *gin.Context) {
	tenantID, ok := tenantFromCtx(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	suites, err := h.cfg.Suites.List(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"suites": suites})
}

// tenantFromCtx mirrors the helper in internal/admin so the eval
// package doesn't have to import admin (would create a cycle —
// admin already imports observability/audit).
func tenantFromCtx(c *gin.Context) (string, bool) {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// retrieveAdapter satisfies the runner's Retriever interface from a
// plain RetrieveFunc.
type retrieveAdapter struct {
	fn RetrieveFunc
}

func (a retrieveAdapter) Retrieve(ctx context.Context, req RetrieveRequest) ([]RetrieveHit, error) {
	return a.fn(ctx, req)
}
