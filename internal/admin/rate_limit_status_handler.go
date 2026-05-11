// Round-12 Task 16 — per-connector rate-limit status endpoint.
//
// GET /v1/admin/sources/:id/rate-limit-status returns the current
// token bucket snapshot for the tenant's source. Operators use
// this when a connector reports slow throughput — the bucket state
// + EffectiveRate + IsThrottled fields immediately distinguish
// "rate-limited by us" from "rate-limited upstream".
//
// Wiring: cmd/api/main.go mounts the handler on the
// /v1/admin/sources group; the route picks up the standard RBAC
// gate from the group's middleware chain.
package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// errNilLimiterStatus is returned when the handler is constructed
// without an inspector.
var errNilLimiterStatus = errors.New("rate-limit-status: nil limiter")

// RateLimitInspector is the narrow read seam the handler needs.
// *RateLimiter implements this in production; tests pass a fake.
type RateLimitInspector interface {
	Inspect(ctx context.Context, tenantID, sourceID string) (RateLimitStatus, error)
}

// RateLimitStatusHandler is the HTTP surface for the endpoint.
type RateLimitStatusHandler struct {
	limiter RateLimitInspector
}

// NewRateLimitStatusHandler returns a handler bound to limiter.
func NewRateLimitStatusHandler(limiter RateLimitInspector) (*RateLimitStatusHandler, error) {
	if limiter == nil {
		return nil, errNilLimiterStatus
	}
	return &RateLimitStatusHandler{limiter: limiter}, nil
}

// Register mounts the handler on g.
func (h *RateLimitStatusHandler) Register(g *gin.RouterGroup) {
	g.GET("/v1/admin/sources/:id/rate-limit-status", h.get)
}

func (h *RateLimitStatusHandler) get(c *gin.Context) {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	tenant, ok := v.(string)
	if !ok || tenant == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	if sourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing source id"})
		return
	}
	status, err := h.limiter.Inspect(c.Request.Context(), tenant, sourceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}
