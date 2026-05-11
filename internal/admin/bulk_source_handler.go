// bulk_source_handler.go — Round-7 Task 10.
//
// POST /v1/admin/sources/bulk applies a single action to a batch
// of sources concurrently. Per-source errors are isolated — a
// failure on one source never aborts the batch.
//
// Supported actions:
//   - pause      → Source.Status = paused
//   - resume     → Source.Status = active
//   - disconnect → Source.Status = removing (lifecycle handed off
//                  to the forget worker)
//
// Each individual operation emits its own audit row so the admin
// timeline reflects the action as if it had been issued via the
// single-source endpoints.
package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// BulkSourceAction enumerates the supported bulk operations.
type BulkSourceAction string

const (
	BulkSourceActionPause      BulkSourceAction = "pause"
	BulkSourceActionResume     BulkSourceAction = "resume"
	BulkSourceActionDisconnect BulkSourceAction = "disconnect"
)

// BulkSourceMutator narrows SourceRepository to the three actions
// the bulk handler needs.
type BulkSourceMutator interface {
	Update(ctx context.Context, tenantID, id string, patch UpdatePatch) (*Source, error)
	MarkRemoving(ctx context.Context, tenantID, id string) (*Source, error)
}

// BulkSourceHandler is the admin HTTP surface.
type BulkSourceHandler struct {
	repo  BulkSourceMutator
	audit AuditWriter
}

// NewBulkSourceHandler validates inputs.
func NewBulkSourceHandler(repo BulkSourceMutator, aw AuditWriter) (*BulkSourceHandler, error) {
	if repo == nil {
		return nil, errors.New("bulk_source: nil repo")
	}
	if aw == nil {
		aw = noopAudit{}
	}
	return &BulkSourceHandler{repo: repo, audit: aw}, nil
}

// Register mounts POST /v1/admin/sources/bulk.
func (h *BulkSourceHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/sources/bulk", h.bulk)
}

// BulkSourceRequest is the JSON body shape.
type BulkSourceRequest struct {
	Action    BulkSourceAction `json:"action" binding:"required"`
	SourceIDs []string         `json:"source_ids" binding:"required"`
}

// BulkSourceResult is the per-source outcome.
type BulkSourceResult struct {
	SourceID string `json:"source_id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// BulkSourceResponse is the JSON envelope.
type BulkSourceResponse struct {
	Action  BulkSourceAction   `json:"action"`
	Total   int                `json:"total"`
	OK      int                `json:"ok"`
	Failed  int                `json:"failed"`
	Results []BulkSourceResult `json:"results"`
}

func (h *BulkSourceHandler) bulk(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req BulkSourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	switch req.Action {
	case BulkSourceActionPause, BulkSourceActionResume, BulkSourceActionDisconnect:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid action"})
		return
	}
	if len(req.SourceIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source_ids cannot be empty"})
		return
	}
	if len(req.SourceIDs) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max 200 source_ids per request"})
		return
	}
	actor := actorIDFromContext(c)
	results := make([]BulkSourceResult, len(req.SourceIDs))
	wg := sync.WaitGroup{}
	for i, sid := range req.SourceIDs {
		i := i
		sid := sid
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := h.apply(c.Request.Context(), tenantID, sid, req.Action)
			res := BulkSourceResult{SourceID: sid, OK: err == nil}
			if err != nil {
				res.Error = err.Error()
			} else {
				h.emit(c.Request.Context(), tenantID, actor, sid, req.Action)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	resp := BulkSourceResponse{Action: req.Action, Total: len(results), Results: results}
	for _, r := range results {
		if r.OK {
			resp.OK++
		} else {
			resp.Failed++
		}
	}
	c.JSON(http.StatusOK, resp)
}

func (h *BulkSourceHandler) apply(ctx context.Context, tenantID, sourceID string, action BulkSourceAction) error {
	switch action {
	case BulkSourceActionPause:
		st := SourceStatusPaused
		_, err := h.repo.Update(ctx, tenantID, sourceID, UpdatePatch{Status: &st})
		return err
	case BulkSourceActionResume:
		st := SourceStatusActive
		_, err := h.repo.Update(ctx, tenantID, sourceID, UpdatePatch{Status: &st})
		return err
	case BulkSourceActionDisconnect:
		_, err := h.repo.MarkRemoving(ctx, tenantID, sourceID)
		return err
	}
	return errors.New("unsupported action")
}

func (h *BulkSourceHandler) emit(ctx context.Context, tenantID, actor, sourceID string, action BulkSourceAction) {
	auditAction := audit.ActionSourceBulkAction
	switch action {
	case BulkSourceActionPause:
		auditAction = audit.ActionSourcePaused
	case BulkSourceActionResume:
		auditAction = audit.ActionSourceResumed
	case BulkSourceActionDisconnect:
		auditAction = audit.ActionSourcePurged
	}
	_ = h.audit.Create(ctx, audit.NewAuditLog(
		tenantID, actor, auditAction, "source", sourceID,
		audit.JSONMap{"bulk": true, "action": string(action)}, "",
	))
}
