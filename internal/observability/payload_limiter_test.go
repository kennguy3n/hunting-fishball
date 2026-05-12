package observability_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

func TestPayloadSizeLimiter_RejectsOversizedContentLength(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.PayloadSizeLimiter(observability.PayloadLimiterConfig{MaxBytes: 64}))
	r.POST("/upload", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	big := strings.Repeat("x", 128)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewBufferString(big))
	req.ContentLength = int64(len(big))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPayloadSizeLimiter_AllowsWithinLimit(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.PayloadSizeLimiter(observability.PayloadLimiterConfig{MaxBytes: 64}))
	r.POST("/upload", func(c *gin.Context) {
		buf := make([]byte, 1024)
		n, _ := c.Request.Body.Read(buf)
		c.String(http.StatusOK, "got %d", n)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewBufferString("small"))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPayloadSizeLimiter_SkipsGET(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.PayloadSizeLimiter(observability.PayloadLimiterConfig{MaxBytes: 1}))
	r.GET("/whatever", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET should bypass limiter; status=%d", w.Code)
	}
}

func TestPayloadSizeLimiter_SkipsConfiguredPaths(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.PayloadSizeLimiter(observability.PayloadLimiterConfig{
		MaxBytes:  1,
		SkipPaths: []string{"/metrics"},
	}))
	r.POST("/metrics", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/metrics", bytes.NewBufferString("anything"))
	req.ContentLength = int64(len("anything"))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics should bypass limiter; status=%d", w.Code)
	}
}

func TestPayloadSizeLimiter_DisabledWhenMaxZero(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.PayloadSizeLimiter(observability.PayloadLimiterConfig{MaxBytes: 0}))
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	w := httptest.NewRecorder()
	big := strings.Repeat("x", 10*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(big))
	req.ContentLength = int64(len(big))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disabled limiter should pass; status=%d", w.Code)
	}
}
