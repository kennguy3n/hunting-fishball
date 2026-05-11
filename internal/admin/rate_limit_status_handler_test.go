// Round-12 Task 16 — tests for the rate-limit status endpoint.
//
// The tests exercise both the happy path (fresh bucket reports
// full capacity) and the throttled path (after exhausting the
// bucket, IsThrottled flips to true). miniredis stands in for
// production Redis.
package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func newStatusLimiter(t *testing.T, lim admin.RateLimit) *admin.RateLimiter {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rl, err := admin.NewRateLimiter(context.Background(), admin.RateLimiterConfig{
		Client: rc, KeyPrefix: "hf:rl", Limit: lim,
	})
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	return rl
}

// newStatusRouter mounts the handler on a tenant-aware group.
func newStatusRouter(t *testing.T, h *admin.RateLimitStatusHandler, tenant string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/", func(c *gin.Context) {
		if tenant != "" {
			c.Set(audit.TenantContextKey, tenant)
		}
		c.Next()
	})
	h.Register(g)
	return r
}

// TestRateLimitStatus_FreshBucketReportsFullCapacity verifies the
// pre-condition that a never-touched bucket has CurrentTokens ==
// MaxTokens and IsThrottled is false.
func TestRateLimitStatus_FreshBucketReportsFullCapacity(t *testing.T) {
	t.Parallel()
	rl := newStatusLimiter(t, admin.RateLimit{Capacity: 10, RefillPerSecond: 2})
	h, err := admin.NewRateLimitStatusHandler(rl)
	if err != nil {
		t.Fatalf("NewRateLimitStatusHandler: %v", err)
	}
	r := newStatusRouter(t, h, "tenant-a")

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/rate-limit-status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got admin.RateLimitStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.TenantID != "tenant-a" || got.SourceID != "src-1" {
		t.Fatalf("tenant/source mismatch: %+v", got)
	}
	if got.MaxTokens != 10 {
		t.Fatalf("MaxTokens=%d, want 10", got.MaxTokens)
	}
	if got.EffectiveRate != 2 {
		t.Fatalf("EffectiveRate=%v, want 2", got.EffectiveRate)
	}
	if got.IsThrottled {
		t.Fatalf("fresh bucket should not be throttled: %+v", got)
	}
	if got.CurrentTokens < 9 {
		// Allow for tiny time-decay during the test but a fresh
		// bucket should be at or near capacity.
		t.Fatalf("CurrentTokens=%v, want ~10", got.CurrentTokens)
	}
}

// TestRateLimitStatus_DrainedBucketReportsThrottled exhausts the
// bucket via Allow() and confirms the status endpoint flips
// IsThrottled to true.
func TestRateLimitStatus_DrainedBucketReportsThrottled(t *testing.T) {
	t.Parallel()
	rl := newStatusLimiter(t, admin.RateLimit{Capacity: 3, RefillPerSecond: 0.01})
	for i := 0; i < 3; i++ {
		ok, _, err := rl.Allow(context.Background(), "tenant-b", "src-2")
		if err != nil || !ok {
			t.Fatalf("burst Allow %d: ok=%v err=%v", i, ok, err)
		}
	}
	// Fourth call drains the bucket below 1.
	_, _, _ = rl.Allow(context.Background(), "tenant-b", "src-2")

	h, _ := admin.NewRateLimitStatusHandler(rl)
	r := newStatusRouter(t, h, "tenant-b")
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-2/rate-limit-status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got admin.RateLimitStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !got.IsThrottled {
		t.Fatalf("drained bucket should be throttled, got: %+v", got)
	}
}

// TestRateLimitStatus_MissingTenantContextReturns401 confirms RBAC
// is not bypassed via the new handler — without a tenant in the
// Gin context the request fails fast.
func TestRateLimitStatus_MissingTenantContextReturns401(t *testing.T) {
	t.Parallel()
	rl := newStatusLimiter(t, admin.RateLimit{Capacity: 10, RefillPerSecond: 1})
	h, _ := admin.NewRateLimitStatusHandler(rl)
	// Empty tenant => middleware doesn't set the context key.
	r := newStatusRouter(t, h, "")
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/rate-limit-status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s; want 401", rec.Code, rec.Body.String())
	}
}
