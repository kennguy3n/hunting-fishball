// slow_query_log_handler.go — Round-14 Task 3.
//
// GET /v1/admin/retrieval/slow-queries lists rows from the
// persistent slow_queries table (migrations/038_slow_queries.sql).
// The handler is paired with the Round-13 endpoint
// /v1/admin/analytics/queries/slow which reads from the
// query_analytics rollup — both are legitimate, but the new
// endpoint isolates the full-detail log so retention and indexing
// can evolve independently.
package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// SlowQueryLogHandler serves the persistent slow-query log.
type SlowQueryLogHandler struct {
	store SlowQueryLister
}

// NewSlowQueryLogHandler binds the handler to store.
func NewSlowQueryLogHandler(store SlowQueryLister) *SlowQueryLogHandler {
	return &SlowQueryLogHandler{store: store}
}

// Register mounts the endpoint on rg.
func (h *SlowQueryLogHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/retrieval/slow-queries", h.list)
}

// SlowQueryLogResponse is the JSON envelope.
type SlowQueryLogResponse struct {
	TenantID string          `json:"tenant_id"`
	Items    []SlowQueryItem `json:"items"`
}

func (h *SlowQueryLogHandler) list(c *gin.Context) {
	tenantID, _ := c.Get(audit.TenantContextKey)
	tid, _ := tenantID.(string)
	if tid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	filter := SlowQueryFilter{TenantID: tid, Limit: 100}
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 500 {
			filter.Limit = v
		}
	}
	if raw := c.Query("since"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			filter.Since = t
		}
	}
	if h.store == nil {
		c.JSON(http.StatusOK, SlowQueryLogResponse{TenantID: tid, Items: []SlowQueryItem{}})
		return
	}
	rows, err := h.store.ListSlowQueries(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	resp := SlowQueryLogResponse{TenantID: tid, Items: make([]SlowQueryItem, 0, len(rows))}
	for _, r := range rows {
		timings := jsonmapToInt64Map(r.BackendTimings)
		resp.Items = append(resp.Items, SlowQueryItem{
			ID:             r.ID,
			QueryHash:      r.QueryHash,
			QueryText:      r.QueryText,
			LatencyMS:      r.LatencyMS,
			HitCount:       r.HitCount,
			TopK:           r.TopK,
			BackendTimings: timings,
			CreatedAt:      r.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, resp)
}

func jsonmapToInt64Map(m JSONMap) map[string]int64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		switch vv := v.(type) {
		case int64:
			out[k] = vv
		case float64:
			out[k] = int64(vv)
		case int:
			out[k] = int64(vv)
		}
	}
	return out
}
