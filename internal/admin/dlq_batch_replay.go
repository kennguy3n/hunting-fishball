// dlq_batch_replay.go — Round-5 Task 11.
//
// POST /v1/admin/dlq/replay batches the existing single-message
// replay endpoint. Operators with hundreds of stuck rows can replay
// a filtered slice in a single request rather than scripting a
// shell loop. The handler hard-caps a batch at MaxReplayBatchSize
// (100) so a single request can't tie up the replayer; clients
// pulling more than that are expected to paginate by adjusting the
// time-range filters.
//
// Filters narrow the working set:
//   - source_id      — limit to one connector instance
//   - min_created_at — RFC3339 inclusive lower bound
//   - max_created_at — RFC3339 exclusive upper bound
//   - error_contains — case-insensitive substring on error_text
//
// Skips are surfaced separately from replays so a 207-style mixed
// outcome is legible. Rows that have hit MaxAttempts surface as
// `skipped: max_attempts`; rows already replayed (without force)
// surface as `skipped: already_replayed`.
package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// MaxReplayBatchSize caps a single batch replay request.
const MaxReplayBatchSize = 100

// DLQBatchReplayRequest is the JSON body of POST /v1/admin/dlq/replay.
type DLQBatchReplayRequest struct {
	SourceID       string `json:"source_id,omitempty"`
	MinCreatedAt   string `json:"min_created_at,omitempty"`
	MaxCreatedAt   string `json:"max_created_at,omitempty"`
	ErrorContains  string `json:"error_contains,omitempty"`
	Force          bool   `json:"force,omitempty"`
	IncludeReplayed bool  `json:"include_replayed,omitempty"`
}

// DLQReplayOutcome reports the per-row result of a batch replay.
type DLQReplayOutcome struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// DLQBatchReplayResponse is the JSON envelope returned to the
// admin.
type DLQBatchReplayResponse struct {
	Replayed   int                `json:"replayed"`
	Skipped    int                `json:"skipped"`
	Considered int                `json:"considered"`
	Outcomes   []DLQReplayOutcome `json:"outcomes"`
}

// RegisterBatchReplay mounts POST /v1/admin/dlq/replay on the
// existing DLQHandler's group. Kept on the handler so it shares
// the same Reader / Replayer / Audit wiring.
func (h *DLQHandler) RegisterBatchReplay(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/dlq/replay", h.batchReplay)
}

func (h *DLQHandler) batchReplay(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	if h.cfg.Replayer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "replay not configured"})
		return
	}

	var req DLQBatchReplayRequest
	_ = c.ShouldBindJSON(&req)

	rows, err := h.cfg.Reader.List(c.Request.Context(), pipeline.DLQListFilter{
		TenantID:        tenantID,
		IncludeReplayed: req.IncludeReplayed || req.Force,
		PageSize:        MaxReplayBatchSize + 1,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filtered, ferr := filterDLQRows(rows, req)
	if ferr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": ferr.Error()})
		return
	}
	if len(filtered) > MaxReplayBatchSize {
		filtered = filtered[:MaxReplayBatchSize]
	}

	resp := DLQBatchReplayResponse{Considered: len(filtered)}
	for i := range filtered {
		row := filtered[i]
		outcome := DLQReplayOutcome{ID: row.ID}
		err := h.cfg.Replayer.Replay(c.Request.Context(), tenantID, row.ID, h.cfg.ReplayTopic, req.Force)
		switch {
		case err == nil:
			outcome.Status = "replayed"
			resp.Replayed++
		case errors.Is(err, pipeline.ErrMaxRetriesExceeded):
			outcome.Status = "skipped"
			outcome.Reason = "max_attempts"
			resp.Skipped++
		case errors.Is(err, pipeline.ErrAlreadyReplayed):
			outcome.Status = "skipped"
			outcome.Reason = "already_replayed"
			resp.Skipped++
		case errors.Is(err, pipeline.ErrDLQNotFound):
			outcome.Status = "skipped"
			outcome.Reason = "not_found"
			resp.Skipped++
		default:
			outcome.Status = "skipped"
			outcome.Reason = err.Error()
			resp.Skipped++
		}
		resp.Outcomes = append(resp.Outcomes, outcome)
	}

	actorID := actorIDFromContext(c)
	_ = h.cfg.Audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actorID, audit.ActionDLQReplayedBatch, "dlq", "",
		audit.JSONMap{
			"replayed":   resp.Replayed,
			"skipped":    resp.Skipped,
			"considered": resp.Considered,
			"force":      req.Force,
		}, "",
	))

	c.JSON(http.StatusOK, resp)
}

// filterDLQRows applies the user-supplied filters to the working
// set returned by the reader. Errors are surfaced when the input
// timestamp shapes are bad — the handler treats them as 400 so the
// admin sees the payload mistake rather than a silently empty
// response.
func filterDLQRows(rows []pipeline.DLQMessage, req DLQBatchReplayRequest) ([]pipeline.DLQMessage, error) {
	var minT, maxT time.Time
	var err error
	if req.MinCreatedAt != "" {
		minT, err = time.Parse(time.RFC3339, req.MinCreatedAt)
		if err != nil {
			return nil, errors.New("min_created_at must be RFC3339")
		}
	}
	if req.MaxCreatedAt != "" {
		maxT, err = time.Parse(time.RFC3339, req.MaxCreatedAt)
		if err != nil {
			return nil, errors.New("max_created_at must be RFC3339")
		}
	}
	needle := strings.ToLower(strings.TrimSpace(req.ErrorContains))

	out := make([]pipeline.DLQMessage, 0, len(rows))
	for _, r := range rows {
		if req.SourceID != "" && r.SourceID != req.SourceID {
			continue
		}
		if !minT.IsZero() && r.CreatedAt.Before(minT) {
			continue
		}
		if !maxT.IsZero() && !r.CreatedAt.Before(maxT) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(r.ErrorText), needle) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// reusable seam for Replayer test injection.
var _ = context.Background
