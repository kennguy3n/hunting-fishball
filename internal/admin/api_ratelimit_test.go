package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type fakeAPILimiter struct {
	mu      sync.Mutex
	allowed map[string]int
	max     int
	retry   time.Duration
	err     error
}

func (f *fakeAPILimiter) Allow(_ context.Context, tenantID, sourceID string) (bool, time.Duration, error) {
	if f.err != nil {
		return false, 0, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := tenantID + "|" + sourceID
	if f.allowed == nil {
		f.allowed = map[string]int{}
	}
	if f.allowed[key] >= f.max {
		return false, f.retry, nil
	}
	f.allowed[key]++
	return true, 0, nil
}

func setupAPIRateRouter(t *testing.T, lim admin.APIRateLimiter, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mw, err := admin.NewAPIRateLimitMiddleware(admin.APIRateLimitMiddlewareConfig{Limiter: lim})
	if err != nil {
		t.Fatalf("NewAPIRateLimitMiddleware: %v", err)
	}
	r := gin.New()
	rg := r.Group("/v1", func(c *gin.Context) {
		if tenantID != "" {
			c.Set(audit.TenantContextKey, tenantID)
		}
		c.Next()
	}, mw)
	rg.GET("/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func TestAPIRateLimit_Validation(t *testing.T) {
	t.Parallel()
	if _, err := admin.NewAPIRateLimitMiddleware(admin.APIRateLimitMiddlewareConfig{}); err == nil {
		t.Fatalf("expected error for nil limiter")
	}
}

func TestAPIRateLimit_AllowsWhenUnderBudget(t *testing.T) {
	t.Parallel()
	r := setupAPIRateRouter(t, &fakeAPILimiter{max: 5}, "tenant-a")
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("req %d: expected 200, got %d", i, w.Code)
		}
	}
}

func TestAPIRateLimit_RejectsAt429WithRetryAfter(t *testing.T) {
	t.Parallel()
	lim := &fakeAPILimiter{max: 1, retry: 2 * time.Second}
	r := setupAPIRateRouter(t, lim, "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After=%q want 2", got)
	}
}

func TestAPIRateLimit_NoTenantBypassed(t *testing.T) {
	t.Parallel()
	lim := &fakeAPILimiter{max: 0}
	r := setupAPIRateRouter(t, lim, "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("missing tenant should bypass limiter, got %d", w.Code)
	}
}

func TestAPIRateLimit_FailsOpenOnLimiterError(t *testing.T) {
	t.Parallel()
	lim := &fakeAPILimiter{err: errors.New("redis down")}
	r := setupAPIRateRouter(t, lim, "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("limiter errors must fail open, got %d", w.Code)
	}
}

func TestAPIRateLimit_PerTenantIsolation(t *testing.T) {
	t.Parallel()
	lim := &fakeAPILimiter{max: 1}
	rA := setupAPIRateRouter(t, lim, "tenant-a")
	rB := setupAPIRateRouter(t, lim, "tenant-b")
	w := httptest.NewRecorder()
	rA.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("A request 1: %d", w.Code)
	}
	w = httptest.NewRecorder()
	rB.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("B request 1 should not be impacted by tenant-a quota: %d", w.Code)
	}
}
