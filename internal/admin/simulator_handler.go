package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// DraftStore is the narrow contract the simulator handler needs
// from the policy.DraftRepository. Real wiring uses the GORM-backed
// repo; tests inject a recorder.
type DraftStore interface {
	Create(ctx context.Context, d *policy.Draft) error
	Get(ctx context.Context, tenantID, id string) (*policy.Draft, error)
	List(ctx context.Context, f policy.DraftListFilter) ([]policy.Draft, error)
}

// PromotionService is the narrow contract for the promotion
// workflow. Real wiring uses *policy.Promoter.
type PromotionService interface {
	PromoteDraft(ctx context.Context, tenantID, draftID, actorID string) (*policy.Draft, error)
	RejectDraft(ctx context.Context, tenantID, draftID, actorID, reason string) (*policy.Draft, error)
}

// SimulatorEngine is the narrow contract for what-if + diff
// simulation. Real wiring uses *policy.Simulator.
type SimulatorEngine interface {
	WhatIf(ctx context.Context, req policy.WhatIfRequest) (*policy.WhatIfResult, error)
	SimulateDataFlow(ctx context.Context, req policy.WhatIfRequest) (*policy.DataFlowDiff, error)
}

// SimulatorConfig configures the policy simulator + drafts handler.
type SimulatorConfig struct {
	Drafts    DraftStore
	Promotion PromotionService
	Simulator SimulatorEngine
	Audit     AuditWriter
}

// SimulatorHandler serves the /v1/admin/policy HTTP API surface:
// draft CRUD + promotion + simulator + conflict detection.
type SimulatorHandler struct {
	cfg SimulatorConfig
}

// NewSimulatorHandler wires the handler. Drafts, Promotion, and
// Simulator are all required; Audit defaults to noopAudit.
func NewSimulatorHandler(cfg SimulatorConfig) (*SimulatorHandler, error) {
	if cfg.Drafts == nil {
		return nil, errors.New("admin: SimulatorHandler requires Drafts")
	}
	if cfg.Promotion == nil {
		return nil, errors.New("admin: SimulatorHandler requires Promotion")
	}
	if cfg.Simulator == nil {
		return nil, errors.New("admin: SimulatorHandler requires Simulator")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	return &SimulatorHandler{cfg: cfg}, nil
}

// Register mounts the policy endpoints on rg:
//
//	POST   /v1/admin/policy/drafts                   — create a draft
//	GET    /v1/admin/policy/drafts                   — list drafts for tenant
//	GET    /v1/admin/policy/drafts/:id               — get a specific draft
//	POST   /v1/admin/policy/drafts/:id/promote       — promote a draft
//	POST   /v1/admin/policy/drafts/:id/reject        — reject a draft
//	POST   /v1/admin/policy/simulate                 — run what-if retrieval
//	POST   /v1/admin/policy/simulate/diff            — run data-flow diff
//	POST   /v1/admin/policy/conflicts                — detect conflicts in a draft
func (h *SimulatorHandler) Register(rg *gin.RouterGroup) {
	g := rg.Group("/v1/admin/policy")
	g.POST("/drafts", h.createDraft)
	g.GET("/drafts", h.listDrafts)
	g.GET("/drafts/:id", h.getDraft)
	g.POST("/drafts/:id/promote", h.promoteDraft)
	g.POST("/drafts/:id/reject", h.rejectDraft)
	g.POST("/simulate", h.simulateWhatIf)
	g.POST("/simulate/diff", h.simulateDataFlow)
	g.POST("/conflicts", h.detectConflicts)
}

// CreateDraftRequest is the POST body. ChannelID is optional
// (empty = tenant-wide draft). Snapshot carries the proposed
// PolicySnapshot.
type CreateDraftRequest struct {
	ChannelID string                `json:"channel_id,omitempty"`
	Snapshot  policy.PolicySnapshot `json:"snapshot"`
}

func (h *SimulatorHandler) createDraft(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req CreateDraftRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	actor := actorIDFromContext(c)
	d := policy.NewDraft(tenantID, req.ChannelID, actor, req.Snapshot)
	if err := h.cfg.Drafts.Create(c.Request.Context(), d); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist: " + err.Error()})
		return
	}

	h.emitDraftAudit(c, d.ID, audit.ActionPolicyDrafted, audit.JSONMap{
		"channel_id": d.ChannelID,
	})
	c.JSON(http.StatusCreated, d)
}

func (h *SimulatorHandler) listDrafts(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	f := policy.DraftListFilter{TenantID: tenantID}
	if status := c.Query("status"); status != "" {
		f.Status = policy.DraftStatus(status)
	}
	drafts, err := h.cfg.Drafts.List(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"drafts": drafts})
}

func (h *SimulatorHandler) getDraft(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	d, err := h.cfg.Drafts.Get(c.Request.Context(), tenantID, id)
	if errors.Is(err, policy.ErrDraftNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "draft not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, d)
}

// PromoteDraftRequest is the POST body for promote. No fields
// today, but reserved so future audit-only metadata
// (e.g. ticket links) can be passed without a breaking change.
type PromoteDraftRequest struct {
	Note string `json:"note,omitempty"`
}

func (h *SimulatorHandler) promoteDraft(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	var req PromoteDraftRequest
	// `ContentLength != 0` (rather than `> 0`) so chunked-encoded
	// bodies — which Go reports as `ContentLength = -1` — are still
	// parsed instead of silently dropped.
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	actor := actorIDFromContext(c)
	d, err := h.cfg.Promotion.PromoteDraft(c.Request.Context(), tenantID, id, actor)
	if errors.Is(err, policy.ErrDraftNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "draft not found"})
		return
	}
	if errors.Is(err, policy.ErrDraftTerminal) {
		c.JSON(http.StatusConflict, gin.H{"error": "draft is in a terminal status"})
		return
	}
	var blocked *policy.ErrPromotionBlocked
	if errors.As(err, &blocked) {
		// 409 Conflict — surfaces the rule list so the admin can
		// fix the draft and retry.
		c.JSON(http.StatusConflict, gin.H{
			"error":     "draft has unresolved error-severity conflicts",
			"conflicts": blocked.Conflicts,
		})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, d)
}

// RejectDraftRequest is the POST body for reject. Reason is
// preserved on the draft row + the audit metadata.
type RejectDraftRequest struct {
	Reason string `json:"reason,omitempty"`
}

func (h *SimulatorHandler) rejectDraft(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	var req RejectDraftRequest
	// `ContentLength != 0` (rather than `> 0`) so chunked-encoded
	// bodies — which Go reports as `ContentLength = -1` — are still
	// parsed instead of silently dropped (which would lose the
	// human-readable rejection reason from the audit trail).
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	actor := actorIDFromContext(c)
	d, err := h.cfg.Promotion.RejectDraft(c.Request.Context(), tenantID, id, actor, req.Reason)
	if errors.Is(err, policy.ErrDraftNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "draft not found"})
		return
	}
	if errors.Is(err, policy.ErrDraftTerminal) {
		c.JSON(http.StatusConflict, gin.H{"error": "draft is in a terminal status"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, d)
}

// SimulateRequest is the POST body for /simulate and /simulate/diff.
// DraftID, when non-empty, hydrates DraftPolicy from the stored
// draft (so an admin can run /simulate with just an id). If both
// are present, DraftID wins so the persisted snapshot is the source
// of truth.
type SimulateRequest struct {
	DraftID     string                `json:"draft_id,omitempty"`
	ChannelID   string                `json:"channel_id,omitempty"`
	UserID      string                `json:"user_id,omitempty"`
	Query       string                `json:"query"`
	TopK        int                   `json:"top_k,omitempty"`
	DraftPolicy policy.PolicySnapshot `json:"draft_policy"`
}

// buildWhatIfRequest hydrates a WhatIfRequest from the incoming
// SimulateRequest. On error it returns the appropriate HTTP status
// code alongside the response body so callers can emit 400 / 404 /
// 500 distinctly — a database fault from Drafts.Get must not be
// swallowed as a 400.
func (h *SimulatorHandler) buildWhatIfRequest(c *gin.Context, tenantID string, req SimulateRequest) (policy.WhatIfRequest, int, gin.H) {
	if req.Query == "" {
		return policy.WhatIfRequest{}, http.StatusBadRequest, gin.H{"error": "query is required"}
	}
	draftPolicy := req.DraftPolicy
	if req.DraftID != "" {
		d, err := h.cfg.Drafts.Get(c.Request.Context(), tenantID, req.DraftID)
		if errors.Is(err, policy.ErrDraftNotFound) {
			return policy.WhatIfRequest{}, http.StatusNotFound, gin.H{"error": "draft not found"}
		}
		if err != nil {
			return policy.WhatIfRequest{}, http.StatusInternalServerError, gin.H{"error": err.Error()}
		}
		draftPolicy = d.Payload.Snapshot
	}
	return policy.WhatIfRequest{
		TenantID:    tenantID,
		ChannelID:   req.ChannelID,
		UserID:      req.UserID,
		Query:       req.Query,
		TopK:        req.TopK,
		DraftPolicy: draftPolicy,
	}, 0, nil
}

func (h *SimulatorHandler) simulateWhatIf(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req SimulateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	whatif, status, errResp := h.buildWhatIfRequest(c, tenantID, req)
	if errResp != nil {
		c.JSON(status, errResp)
		return
	}
	res, err := h.cfg.Simulator.WhatIf(c.Request.Context(), whatif)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *SimulatorHandler) simulateDataFlow(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req SimulateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	whatif, status, errResp := h.buildWhatIfRequest(c, tenantID, req)
	if errResp != nil {
		c.JSON(status, errResp)
		return
	}
	diff, err := h.cfg.Simulator.SimulateDataFlow(c.Request.Context(), whatif)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, diff)
}

// ConflictsRequest accepts either a DraftID (hydrates the snapshot
// from the store) or a raw Snapshot for ad-hoc evaluation from the
// admin portal's draft editor before the row is persisted.
type ConflictsRequest struct {
	DraftID  string                `json:"draft_id,omitempty"`
	Snapshot policy.PolicySnapshot `json:"snapshot"`
}

func (h *SimulatorHandler) detectConflicts(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req ConflictsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	snap := req.Snapshot
	if req.DraftID != "" {
		d, err := h.cfg.Drafts.Get(c.Request.Context(), tenantID, req.DraftID)
		if errors.Is(err, policy.ErrDraftNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "draft not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		snap = d.Payload.Snapshot
	}
	conflicts := policy.DetectConflicts(snap)
	c.JSON(http.StatusOK, gin.H{
		"conflicts":  conflicts,
		"has_error":  policy.HasErrors(conflicts),
		"checked_at": time.Now().UTC(),
	})
}

func (h *SimulatorHandler) emitDraftAudit(c *gin.Context, draftID string, action audit.Action, meta audit.JSONMap) {
	tenantID, _ := tenantIDFromContext(c)
	actorID := actorIDFromContext(c)
	log := audit.NewAuditLog(tenantID, actorID, action, "policy_draft", draftID, meta, "")
	_ = h.cfg.Audit.Create(c.Request.Context(), log)
}
