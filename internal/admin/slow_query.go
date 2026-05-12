// slow_query.go — Round-13 Task 8.
//
// Admin endpoint that surfaces the rows the retrieval handler
// flagged with slow=true. The retrieval-side detection lives in
// internal/retrieval/slow_query.go; the persistence column lives
// on query_analytics (migration 036_query_analytics_slow.sql).
//
// The handler is intentionally tenant-scoped: an admin from
// tenant-a cannot see slow queries from tenant-b. The optional
// since=… query parameter lets operators drill into a specific
// outage window without pulling the entire table.
package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// SlowQueryHandler serves GET /v1/admin/analytics/queries/slow.
type SlowQueryHandler struct {
	store QueryAnalyticsStore
}

// NewSlowQueryHandler wires the handler to the supplied store.
func NewSlowQueryHandler(store QueryAnalyticsStore) *SlowQueryHandler {
	return &SlowQueryHandler{store: store}
}

// Register mounts the endpoint on rg.
func (h *SlowQueryHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/analytics/queries/slow", h.list)
}

// SlowQueryItem is the per-row projection.
type SlowQueryItem struct {
	ID             string           `json:"id"`
	QueryHash      string           `json:"query_hash"`
	QueryText      string           `json:"query_text"`
	LatencyMS      int              `json:"latency_ms"`
	HitCount       int              `json:"hit_count"`
	TopK           int              `json:"top_k"`
	BackendTimings map[string]int64 `json:"backend_timings,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// SlowQueryResponse is the JSON envelope.
type SlowQueryResponse struct {
	TenantID string          `json:"tenant_id"`
	Items    []SlowQueryItem `json:"items"`
}

func (h *SlowQueryHandler) list(c *gin.Context) {
	tenantID, _ := c.Get(audit.TenantContextKey)
	tid, _ := tenantID.(string)
	if tid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	q := QueryAnalyticsQuery{TenantID: tid, SlowOnly: true, Limit: 100}
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 1000 {
			q.Limit = v
		}
	}
	if raw := c.Query("since"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			q.Since = t
		}
	}
	rows, err := h.store.List(c.Request.Context(), q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	resp := SlowQueryResponse{TenantID: tid, Items: make([]SlowQueryItem, 0, len(rows))}
	for _, r := range rows {
		var timings map[string]int64
		if len(r.BackendTimings) > 0 {
			timings = make(map[string]int64, len(r.BackendTimings))
			for k, v := range r.BackendTimings {
				switch vv := v.(type) {
				case int64:
					timings[k] = vv
				case float64:
					timings[k] = int64(vv)
				case int:
					timings[k] = int64(vv)
				}
			}
		}
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
