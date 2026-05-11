// cache_stats_handler.go — Round-13 Task 9.
//
// Semantic-cache hit-rate per tenant over a configurable time
// window. The cluster-wide Prometheus counters
// (context_engine_retrieval_cache_hits_total /
// _misses_total) deliberately carry no tenant label per the
// metric cardinality policy in
// internal/observability/metrics.go. To get the per-tenant
// breakdown without explosion, we aggregate from
// query_analytics.cache_hit instead, which already carries a
// tenant_id label and a cache_hit boolean.
//
// The endpoint returns one row per tenant the caller is allowed
// to see — tenant scope is enforced by the existing audit
// middleware, so the response only carries the caller's tenant.
package admin

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// CacheStatsHandler serves GET /v1/admin/analytics/cache-stats.
type CacheStatsHandler struct {
	store QueryAnalyticsStore
}

// NewCacheStatsHandler wires the handler to the supplied store.
func NewCacheStatsHandler(store QueryAnalyticsStore) *CacheStatsHandler {
	return &CacheStatsHandler{store: store}
}

// Register mounts the endpoint on rg.
func (h *CacheStatsHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/analytics/cache-stats", h.get)
}

// CacheStatsResponse is the JSON envelope.
type CacheStatsResponse struct {
	TenantID     string    `json:"tenant_id"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	Hits         int       `json:"hits"`
	Misses       int       `json:"misses"`
	Total        int       `json:"total"`
	HitRatePct   float64   `json:"hit_rate_pct"`
	MissRatePct  float64   `json:"miss_rate_pct"`
	WindowMin    int       `json:"window_minutes"`
}

func (h *CacheStatsHandler) get(c *gin.Context) {
	tenantID, _ := c.Get(audit.TenantContextKey)
	tid, _ := tenantID.(string)
	if tid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	// Default window: 60 minutes.
	windowMin := 60
	if raw := c.Query("window_minutes"); raw != "" {
		if d, err := time.ParseDuration(raw + "m"); err == nil && d > 0 && d <= 24*time.Hour {
			windowMin = int(d.Minutes())
		}
	}
	end := time.Now().UTC()
	start := end.Add(-time.Duration(windowMin) * time.Minute)
	rows, err := h.store.List(c.Request.Context(), QueryAnalyticsQuery{
		TenantID: tid, Since: start, Until: end, Limit: 1000,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	resp := CacheStatsResponse{
		TenantID:    tid,
		WindowStart: start,
		WindowEnd:   end,
		WindowMin:   windowMin,
	}
	for _, r := range rows {
		if r.CacheHit {
			resp.Hits++
		} else {
			resp.Misses++
		}
	}
	resp.Total = resp.Hits + resp.Misses
	if resp.Total > 0 {
		resp.HitRatePct = float64(resp.Hits) / float64(resp.Total) * 100
		resp.MissRatePct = float64(resp.Misses) / float64(resp.Total) * 100
	}
	c.JSON(http.StatusOK, resp)
}
