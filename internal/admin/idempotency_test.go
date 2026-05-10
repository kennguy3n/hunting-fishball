package admin_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func newIdempotencyRig(t *testing.T) (*gin.Engine, *miniredis.Miniredis, *atomic.Int64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	mw, err := admin.NewIdempotencyMiddleware(admin.IdempotencyMiddlewareConfig{Store: rc})
	if err != nil {
		t.Fatalf("NewIdempotencyMiddleware: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	r.Use(mw)
	calls := &atomic.Int64{}
	r.POST("/v1/admin/things", func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusCreated, gin.H{"call": calls.Load()})
	})
	r.GET("/v1/admin/things", func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusOK, gin.H{"call": calls.Load()})
	})
	return r, mr, calls
}

func TestIdempotency_FirstRequestRunsHandler(t *testing.T) {
	t.Parallel()
	r, _, calls := newIdempotencyRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/things", bytes.NewReader([]byte(`{}`)))
	req.Header.Set(admin.IdempotencyHeader, "key-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 handler call, got %d", calls.Load())
	}
}

func TestIdempotency_RepeatedRequestReplaysCachedResponse(t *testing.T) {
	t.Parallel()
	r, _, calls := newIdempotencyRig(t)
	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/things", bytes.NewReader([]byte(`{}`)))
		req.Header.Set(admin.IdempotencyHeader, "key-2")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	first := doReq()
	if first.Code != http.StatusCreated {
		t.Fatalf("first status: %d", first.Code)
	}
	second := doReq()
	if second.Code != http.StatusCreated {
		t.Fatalf("second status: %d", second.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler ran twice; got %d calls", calls.Load())
	}
	if second.Header().Get("X-Idempotency-Replayed") != "true" {
		t.Fatalf("missing replay header on second response: %s", second.Header())
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("replayed body differs:\nfirst=%s\nsecond=%s", first.Body, second.Body)
	}
}

func TestIdempotency_DifferentKeysAreIndependent(t *testing.T) {
	t.Parallel()
	r, _, calls := newIdempotencyRig(t)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/things", bytes.NewReader([]byte(`{}`)))
		req.Header.Set(admin.IdempotencyHeader, "k-"+strconv.Itoa(i))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("[%d] status: %d", i, w.Code)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 distinct calls, got %d", calls.Load())
	}
}

func TestIdempotency_GetIsBypassed(t *testing.T) {
	t.Parallel()
	r, _, calls := newIdempotencyRig(t)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/things", nil)
		req.Header.Set(admin.IdempotencyHeader, "k-get")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d", w.Code)
		}
	}
	// GET requests are not subject to caching, so handler runs twice.
	if calls.Load() != 2 {
		t.Fatalf("expected 2 GET calls, got %d", calls.Load())
	}
}

func TestIdempotency_NoHeaderIsBypassed(t *testing.T) {
	t.Parallel()
	r, _, calls := newIdempotencyRig(t)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/things", bytes.NewReader([]byte(`{}`)))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("status: %d", w.Code)
		}
	}
	// No idempotency header → middleware does not cache, handler always runs.
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
}

// stubFailingStore makes every Get/SetNX call return an error so we
// can verify the middleware falls open instead of returning 5xx.
type stubFailingStore struct{}

func (stubFailingStore) Get(_ context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	cmd.SetErr(errors.New("redis offline"))
	return cmd
}

func (stubFailingStore) SetNX(_ context.Context, _ string, _ any, _ time.Duration) *redis.BoolCmd {
	cmd := redis.NewBoolCmd(context.Background())
	cmd.SetErr(errors.New("redis offline"))
	return cmd
}

func TestIdempotency_FailsOpenOnRedisError(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	mw, err := admin.NewIdempotencyMiddleware(admin.IdempotencyMiddlewareConfig{Store: stubFailingStore{}})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	r.Use(mw)
	calls := &atomic.Int64{}
	r.POST("/v1/admin/things", func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusCreated, gin.H{"ok": true})
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/things", bytes.NewReader([]byte(`{}`)))
		req.Header.Set(admin.IdempotencyHeader, "k")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
		}
	}
	// Falling open means handler ran both times despite Redis errors.
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls under fail-open, got %d", calls.Load())
	}
}

func TestIdempotency_TenantsIndependent(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	mw, _ := admin.NewIdempotencyMiddleware(admin.IdempotencyMiddlewareConfig{Store: rc})
	calls := &atomic.Int64{}
	build := func(tenantID string) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(audit.TenantContextKey, tenantID)
			c.Next()
		})
		r.Use(mw)
		r.POST("/v1/admin/things", func(c *gin.Context) {
			calls.Add(1)
			c.JSON(http.StatusCreated, gin.H{"tenant": tenantID})
		})
		return r
	}
	rA := build("tenant-a")
	rB := build("tenant-b")
	doReq := func(r *gin.Engine) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/things", bytes.NewReader([]byte(`{}`)))
		req.Header.Set(admin.IdempotencyHeader, "shared-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	respA := doReq(rA)
	respB := doReq(rB)
	if calls.Load() != 2 {
		t.Fatalf("expected 2 distinct calls (one per tenant), got %d", calls.Load())
	}
	if respA.Body.String() == respB.Body.String() {
		t.Fatalf("tenants returned identical bodies — cache key not tenant-scoped")
	}
}
