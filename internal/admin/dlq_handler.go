// dlq_handler.go — Phase 8 / Task 5 admin surface for the persistent
// DLQ. The handler exposes:
//
//	GET  /v1/admin/dlq                — list failed messages (cursor-paginated)
//	GET  /v1/admin/dlq/:id            — fetch one row
//	POST /v1/admin/dlq/:id/replay     — re-inject the row's event onto the main topic
//
// Tenant scoping is identical to the rest of the admin surface:
// tenant_id comes from the gin context and is never read from the
// request body or path.
package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// DLQReader is the narrow read-side contract the handler needs.
// *pipeline.DLQStoreGORM satisfies it.
type DLQReader interface {
	List(ctx context.Context, filter pipeline.DLQListFilter) ([]pipeline.DLQMessage, error)
	Get(ctx context.Context, tenantID, id string) (*pipeline.DLQMessage, error)
}

// DLQReplayer is the narrow contract the handler invokes when an
// operator clicks "Replay" in the admin portal. *pipeline.Replayer
// satisfies it.
type DLQReplayer interface {
	Replay(ctx context.Context, tenantID, id, topic string, force bool) error
	MaxAttempts() int
}

// DLQHandlerConfig configures a DLQHandler.
type DLQHandlerConfig struct {
	// Reader is the storage seam. Required.
	Reader DLQReader

	// Replayer publishes the row's IngestEvent back onto the main
	// topic. Optional — when nil the replay endpoint returns 503.
	Replayer DLQReplayer

	// ReplayTopic is the destination topic for replays (typically
	// "ingest"). Required when Replayer is non-nil.
	ReplayTopic string

	// Audit, when non-nil, records every replay attempt.
	Audit AuditWriter
}

// DLQHandler serves /v1/admin/dlq.
type DLQHandler struct {
	cfg DLQHandlerConfig
}

// NewDLQHandler validates cfg and returns a handler.
func NewDLQHandler(cfg DLQHandlerConfig) (*DLQHandler, error) {
	if cfg.Reader == nil {
		return nil, errors.New("admin dlq: nil Reader")
	}
	if cfg.Replayer != nil && cfg.ReplayTopic == "" {
		return nil, errors.New("admin dlq: ReplayTopic required when Replayer set")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	return &DLQHandler{cfg: cfg}, nil
}

// Register mounts the routes on rg.
func (h *DLQHandler) Register(rg *gin.RouterGroup) {
	g := rg.Group("/v1/admin/dlq")
	g.GET("", h.list)
	g.GET("/:id", h.get)
	g.POST("/:id/replay", h.replay)
}

// DLQReplayRequest is the optional body for POST /v1/admin/dlq/:id/replay.
type DLQReplayRequest struct {
	// Force, when true, replays a row that has already been
	// replayed at least once. Default false.
	Force bool `json:"force,omitempty"`
}

func (h *DLQHandler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	filter := pipeline.DLQListFilter{
		TenantID:      tenantID,
		OriginalTopic: c.Query("original_topic"),
		// Round-5 Task 3 alias: `cursor` is the canonical name; we
		// keep `page_token` working for backwards compatibility.
		PageToken: firstNonEmpty(c.Query("cursor"), c.Query("page_token")),
	}
	if v := c.Query("include_replayed"); v != "" {
		filter.IncludeReplayed = v == "true" || v == "1"
	}
	rawPageSize := firstNonEmpty(c.Query("limit"), c.Query("page_size"))
	if rawPageSize != "" {
		n, err := strconv.Atoi(rawPageSize)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		if n > MaxPageLimit {
			n = MaxPageLimit
		}
		filter.PageSize = n
	}
	rows, err := h.cfg.Reader.List(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Fetch-N+1 pagination: the store returns up to
	// effectivePageSize+1 rows. We only emit next_page_token when the
	// store actually returned more than the caller asked for. This
	// avoids two prior bugs:
	//   1) page_size unset — every non-empty response would emit a
	//      token because the old guard short-circuited on
	//      filter.PageSize == 0, forcing clients to make at least one
	//      extra empty round-trip.
	//   2) page_size set exactly to the page boundary — a last page
	//      that ended on a multiple of pageSize would still emit a
	//      phantom token because the old guard used >= rather than >.
	effectivePageSize := pipeline.EffectiveDLQPageSize(filter.PageSize)
	var nextToken string
	if len(rows) > effectivePageSize {
		nextToken = rows[effectivePageSize-1].ID
		rows = rows[:effectivePageSize]
	}
	c.JSON(http.StatusOK, gin.H{
		"items":           rows,
		"next_page_token": nextToken,
		"next_cursor":     nextToken,
	})
}

func (h *DLQHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	row, err := h.cfg.Reader.Get(c.Request.Context(), tenantID, id)
	if errors.Is(err, pipeline.ErrDLQNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "dlq message not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}

func (h *DLQHandler) replay(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	if h.cfg.Replayer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "replay not configured"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	var req DLQReplayRequest
	_ = c.ShouldBindJSON(&req)

	err := h.cfg.Replayer.Replay(c.Request.Context(), tenantID, id, h.cfg.ReplayTopic, req.Force)
	switch {
	case errors.Is(err, pipeline.ErrDLQNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "dlq message not found"})
		return
	case errors.Is(err, pipeline.ErrAlreadyReplayed):
		c.JSON(http.StatusConflict, gin.H{"error": "already replayed; pass force=true to replay again"})
		return
	case errors.Is(err, pipeline.ErrMaxRetriesExceeded):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":        "max replay attempts exceeded",
			"max_attempts": h.cfg.Replayer.MaxAttempts(),
		})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actorID := actorIDFromContext(c)
	_ = h.cfg.Audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actorID, audit.ActionDLQReplayed, "dlq", id,
		audit.JSONMap{"force": req.Force, "replay_topic": h.cfg.ReplayTopic}, "",
	))
	c.JSON(http.StatusAccepted, gin.H{"id": id, "status": "replayed"})
}
