// reindex_handler.go — Phase 8 / Task 7 admin endpoint.
//
//	POST /v1/admin/reindex   { "source_id": "...", "namespace_id": "..." }
//
// The handler validates the request, resolves the tenant from the
// gin context, and asks the configured pipeline.ReindexOrchestrator
// to fan one EventReindex per owned document onto the main topic.
package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// ReindexRunner is the narrow contract the handler invokes.
// *pipeline.ReindexOrchestrator satisfies it.
type ReindexRunner interface {
	Reindex(ctx context.Context, req pipeline.ReindexRequest) (pipeline.ReindexResult, error)
}

// ReindexHandlerConfig configures a ReindexHandler.
type ReindexHandlerConfig struct {
	Runner ReindexRunner
	Audit  AuditWriter
}

// ReindexHandler serves /v1/admin/reindex.
type ReindexHandler struct {
	cfg ReindexHandlerConfig
}

// NewReindexHandler validates cfg.
func NewReindexHandler(cfg ReindexHandlerConfig) (*ReindexHandler, error) {
	if cfg.Runner == nil {
		return nil, errors.New("admin reindex: nil Runner")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	return &ReindexHandler{cfg: cfg}, nil
}

// Register mounts the handler.
func (h *ReindexHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/reindex", h.reindex)
}

// ReindexAdminRequest is the JSON body. Distinct from
// pipeline.ReindexRequest because the API never accepts a
// tenant_id from the client — the tenant is resolved from auth.
type ReindexAdminRequest struct {
	SourceID    string `json:"source_id" binding:"required"`
	NamespaceID string `json:"namespace_id,omitempty"`
}

// ReindexAdminResponse mirrors pipeline.ReindexResult.
type ReindexAdminResponse struct {
	DocumentsEnumerated int `json:"documents_enumerated"`
	EventsEmitted       int `json:"events_emitted"`
	EmitErrors          int `json:"emit_errors"`
}

func (h *ReindexHandler) reindex(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req ReindexAdminRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.cfg.Runner.Reindex(c.Request.Context(), pipeline.ReindexRequest{
		TenantID:    tenantID,
		SourceID:    req.SourceID,
		NamespaceID: req.NamespaceID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actorID := actorIDFromContext(c)
	_ = h.cfg.Audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actorID, audit.ActionReindexRequested, "source", req.SourceID,
		audit.JSONMap{"namespace_id": req.NamespaceID, "events_emitted": res.EventsEmitted, "documents_enumerated": res.DocumentsEnumerated},
		"",
	))
	c.JSON(http.StatusAccepted, ReindexAdminResponse{
		DocumentsEnumerated: res.DocumentsEnumerated,
		EventsEmitted:       res.EventsEmitted,
		EmitErrors:          res.EmitErrors,
	})
}
