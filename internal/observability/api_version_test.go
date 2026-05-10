package observability_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

func newRouter(versions ...string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := observability.APIVersionConfig{}
	if len(versions) > 0 {
		cfg.Supported = versions
	}
	r.Use(observability.APIVersionMiddleware(cfg))
	r.GET("/v1/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": c.GetString(observability.APIVersionContextKey)})
	})
	r.GET("/v2/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": c.GetString(observability.APIVersionContextKey)})
	})
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": c.GetString(observability.APIVersionContextKey)})
	})
	return r
}

func TestAPIVersionMiddleware_V1Path(t *testing.T) {
	r := newRouter()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if got := rr.Header().Get("X-API-Version"); got != "v1" {
		t.Fatalf("header=%s", got)
	}
}

func TestAPIVersionMiddleware_UnknownVersionRejected(t *testing.T) {
	r := newRouter()
	req := httptest.NewRequest(http.MethodGet, "/v2/ping", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotAcceptable {
		t.Fatalf("expected 406; got %d", rr.Code)
	}
}

func TestAPIVersionMiddleware_DefaultsToV1(t *testing.T) {
	r := newRouter()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if got := rr.Header().Get("X-API-Version"); got != "v1" {
		t.Fatalf("expected v1 default; got %s", got)
	}
}

func TestAPIVersionMiddleware_AcceptVersionHeader(t *testing.T) {
	r := newRouter("v1", "v2")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Accept-Version", "v2")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if got := rr.Header().Get("X-API-Version"); got != "v2" {
		t.Fatalf("expected v2; got %s", got)
	}
}

func TestAPIVersionMiddleware_HeaderRejectedIfUnknown(t *testing.T) {
	r := newRouter()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Accept-Version", "v9")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotAcceptable {
		t.Fatalf("expected 406; got %d", rr.Code)
	}
}
