// query_analytics_handler.go — Round-7 Task 4.
//
// GET /v1/admin/analytics/queries reports recent retrieval queries
// for the calling tenant. Supports `since`, `until`, and `top`
// query-string filters. The top-N projection groups by query_hash
// and reports average latency / hit count / cache hit ratio.
package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// QueryAnalyticsHandler is the admin HTTP surface.
type QueryAnalyticsHandler struct {
	store QueryAnalyticsStore
}

// NewQueryAnalyticsHandler validates inputs.
func NewQueryAnalyticsHandler(store QueryAnalyticsStore) (*QueryAnalyticsHandler, error) {
	if store == nil {
		return nil, errors.New("query_analytics: nil store")
	}
	return &QueryAnalyticsHandler{store: store}, nil
}

// Register mounts the routes.
//
//	GET /v1/admin/analytics/queries
//	GET /v1/admin/analytics/queries/top
func (h *QueryAnalyticsHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/analytics/queries", h.list)
	rg.GET("/v1/admin/analytics/queries/top", h.top)
}

func (h *QueryAnalyticsHandler) parseQuery(c *gin.Context) (QueryAnalyticsQuery, bool) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return QueryAnalyticsQuery{}, false
	}
	q := QueryAnalyticsQuery{TenantID: tenantID}
	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return QueryAnalyticsQuery{}, false
		}
		q.Since = t
	}
	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return QueryAnalyticsQuery{}, false
		}
		q.Until = t
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a non-negative integer"})
			return QueryAnalyticsQuery{}, false
		}
		q.Limit = n
	}
	return q, true
}

func (h *QueryAnalyticsHandler) list(c *gin.Context) {
	q, ok := h.parseQuery(c)
	if !ok {
		return
	}
	rows, err := h.store.List(c.Request.Context(), q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": rows, "count": len(rows)})
}

func (h *QueryAnalyticsHandler) top(c *gin.Context) {
	q, ok := h.parseQuery(c)
	if !ok {
		return
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	top, err := h.store.TopQueries(c.Request.Context(), q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"top": top, "count": len(top)})
}
