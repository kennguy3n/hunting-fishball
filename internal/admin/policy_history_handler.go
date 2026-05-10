// policy_history_handler.go — Round-5 Task 15.
//
// PolicyHistoryHandler exposes the policy_versions audit trail
// over two endpoints:
//
//   - GET  /v1/admin/policy/history          — paginated listing
//     of every promote/reject for the calling tenant. Supports the
//     same cursor/limit shape as Task 3 (default 50, max 200,
//     ULID-descending so newest first; empty next_cursor means
//     no more pages).
//
//   - POST /v1/admin/policy/rollback/:version_id — recreates a
//     fresh draft from the historical snapshot and immediately
//     promotes it. The original version row stays in place; a
//     new version row is appended with action=policy.rolled_back
//     so the rollback itself is auditable.
//
// Both endpoints are RBAC-gated: GET requires viewer-or-admin,
// POST requires admin. Tenant scoping is enforced server-side —
// the caller's tenant_id is read from the Gin context, never from
// the URL/body, so a tenant cannot reference another tenant's
// version-id.
package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// PolicyHistoryConfig wires the handler.
type PolicyHistoryConfig struct {
	Versions *policy.PolicyVersionRepository
	Drafts   *policy.DraftRepository
	Promoter *policy.Promoter
	Audit    AuditWriter
}

// PolicyHistoryHandler exposes the policy_versions endpoints.
type PolicyHistoryHandler struct {
	cfg PolicyHistoryConfig
}

// NewPolicyHistoryHandler validates cfg and returns the handler.
func NewPolicyHistoryHandler(cfg PolicyHistoryConfig) (*PolicyHistoryHandler, error) {
	if cfg.Versions == nil {
		return nil, errors.New("policy_history: missing Versions")
	}
	if cfg.Drafts == nil {
		return nil, errors.New("policy_history: missing Drafts")
	}
	if cfg.Promoter == nil {
		return nil, errors.New("policy_history: missing Promoter")
	}
	if cfg.Audit == nil {
		return nil, errors.New("policy_history: missing Audit")
	}
	return &PolicyHistoryHandler{cfg: cfg}, nil
}

// Register mounts the routes onto the supplied router. The
// expected mount path is /v1/admin so the resulting endpoints
// are /v1/admin/policy/history and /v1/admin/policy/rollback/...
func (h *PolicyHistoryHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/policy/history", h.list)
	rg.POST("/policy/rollback/:version_id", h.rollback)
}

// PolicyHistoryListResponse mirrors the Task 3 envelope shape.
type PolicyHistoryListResponse struct {
	Items      []policy.PolicyVersion `json:"items"`
	NextCursor string                 `json:"next_cursor"`
}

func (h *PolicyHistoryHandler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	limit, err := parsePageLimit(c.Query("limit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if limit == 0 {
		limit = DefaultPageLimit
	}
	cursor := firstNonEmpty(c.Query("cursor"), c.Query("page_token"))
	// Fetch-N+1 pagination: ask the repo for one more row than the
	// caller's limit so we can detect whether another page exists
	// without an extra COUNT(*). When the repo returns >limit rows
	// we trim to limit and emit next_cursor; when it returns ≤limit
	// rows the result is the final page and next_cursor stays empty.
	// Mirrors the pattern used by source_repository.go:129-141 and
	// dlq_handler.go:124-140 — fixes the prior bug where a dataset
	// whose size was an exact multiple of `limit` emitted a phantom
	// cursor that resolved to an empty next page.
	rows, err := h.cfg.Versions.List(c.Request.Context(), policy.VersionListFilter{
		TenantID:  tenantID,
		ChannelID: c.Query("channel_id"),
		Cursor:    cursor,
		Limit:     limit + 1,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp := PolicyHistoryListResponse{Items: rows}
	if len(rows) > limit {
		resp.Items = rows[:limit]
		resp.NextCursor = resp.Items[len(resp.Items)-1].ID
	}
	c.JSON(http.StatusOK, resp)
}

// PolicyRollbackResponse is the rollback handler's success body.
type PolicyRollbackResponse struct {
	VersionID  string        `json:"version_id"`
	NewDraftID string        `json:"new_draft_id"`
	PromotedAt time.Time     `json:"promoted_at"`
	Draft      *policy.Draft `json:"draft"`
}

func (h *PolicyHistoryHandler) rollback(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	versionID := c.Param("version_id")
	if versionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing version_id"})
		return
	}
	v, err := h.cfg.Versions.Get(c.Request.Context(), tenantID, versionID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "version not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actorID := actorIDFromContext(c)

	// Build a fresh draft from the historical snapshot. The
	// snapshot column is the same DraftPayload shape as
	// Draft.Payload so we can lift it directly without
	// re-serialising.
	d := policy.NewDraft(tenantID, v.ChannelID, actorID, v.Snapshot.Snapshot)
	if err := h.cfg.Drafts.Create(c.Request.Context(), d); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Auto-promote the new draft. The Promoter records its own
	// policy_versions row (action=policy.promoted) in the same
	// transaction; we additionally emit a policy.rolled_back
	// audit row outside that tx so the timeline has both events
	// — promotion (operational) and rollback (intent).
	promoted, err := h.cfg.Promoter.PromoteDraft(c.Request.Context(), tenantID, d.ID, actorID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.cfg.Audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actorID, audit.ActionPolicyRolledBack, "policy_version", versionID,
		audit.JSONMap{
			"new_draft_id": d.ID,
			"channel_id":   v.ChannelID,
			"original_at":  v.CreatedAt.UTC().Format(time.RFC3339Nano),
		},
		"",
	))

	resp := PolicyRollbackResponse{
		VersionID:  versionID,
		NewDraftID: d.ID,
		Draft:      promoted,
	}
	if promoted != nil && promoted.PromotedAt != nil {
		resp.PromotedAt = *promoted.PromotedAt
	}
	c.JSON(http.StatusOK, resp)
}
