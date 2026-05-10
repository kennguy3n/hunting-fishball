// api_ratelimit.go — Phase 8 / Task 8 public-API rate limiting.
//
// The middleware reuses the existing Redis-backed token bucket
// (`ratelimit.go`) but addresses one bucket per (tenant_id, "api")
// instead of (tenant_id, source_id). Mounted onto the /v1/ Gin
// group in cmd/api/main.go behind the auth middleware so the bucket
// key is always derived from a verified tenant context.
//
// Limit is configured via CONTEXT_ENGINE_API_RATE_LIMIT (sustained
// requests-per-second per tenant). When the bucket is empty the
// middleware returns 429 with a Retry-After header.
package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// APIRateLimiter is the narrow contract the middleware uses. Both
// *RateLimiter and the in-memory test fake satisfy it.
type APIRateLimiter interface {
	Allow(ctx context.Context, tenantID, sourceID string) (bool, time.Duration, error)
}

// APIRateLimitMiddlewareConfig configures NewAPIRateLimitMiddleware.
type APIRateLimitMiddlewareConfig struct {
	Limiter APIRateLimiter
	// Bucket scopes the bucket suffix; defaults to "api".
	Bucket string
}

// NewAPIRateLimitMiddleware returns a Gin middleware that enforces
// the configured per-tenant rate limit on every request. Requests
// without a tenant context fall through unchecked because the auth
// middleware in cmd/api/main.go already rejects unauthenticated
// callers — the limiter is downstream of auth on purpose.
func NewAPIRateLimitMiddleware(cfg APIRateLimitMiddlewareConfig) (gin.HandlerFunc, error) {
	if cfg.Limiter == nil {
		return nil, errors.New("api ratelimit: nil Limiter")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "api"
	}
	return func(c *gin.Context) {
		tenantIDRaw, exists := c.Get(audit.TenantContextKey)
		if !exists {
			c.Next()
			return
		}
		tenantID, _ := tenantIDRaw.(string)
		if tenantID == "" {
			c.Next()
			return
		}
		allowed, retryAfter, err := cfg.Limiter.Allow(c.Request.Context(), tenantID, cfg.Bucket)
		if err != nil {
			c.Next()
			return
		}
		if !allowed {
			retrySecs := int(retryAfter.Seconds())
			if retrySecs <= 0 {
				retrySecs = 1
			}
			c.Header("Retry-After", strconv.Itoa(retrySecs))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate limit exceeded",
				"retry_after": retryAfter.Milliseconds(),
			})
			return
		}
		c.Next()
	}, nil
}
