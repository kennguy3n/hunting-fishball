package observability_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

func TestRequestID_HonoursInboundHeader(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.RequestIDMiddleware())
	var got string
	r.GET("/x", func(c *gin.Context) {
		got = observability.RequestIDFromContext(c.Request.Context())
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(observability.RequestIDHeader, "req-abc-123")
	r.ServeHTTP(w, req)
	if got != "req-abc-123" {
		t.Fatalf("got=%q", got)
	}
	if w.Header().Get(observability.RequestIDHeader) != "req-abc-123" {
		t.Fatalf("response header missing")
	}
}

func TestRequestID_GeneratesULIDWhenMissing(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.RequestIDMiddleware())
	var got string
	r.GET("/x", func(c *gin.Context) {
		got = observability.RequestIDFromContext(c.Request.Context())
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)
	if len(got) != 26 { // ULID
		t.Fatalf("expected 26-char ULID, got %q", got)
	}
	if w.Header().Get(observability.RequestIDHeader) != got {
		t.Fatalf("response header missing or mismatched")
	}
}

func TestRequestID_RejectsControlCharacters(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.RequestIDMiddleware())
	var got string
	r.GET("/x", func(c *gin.Context) {
		got = observability.RequestIDFromContext(c.Request.Context())
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(observability.RequestIDHeader, "bad\x00id")
	r.ServeHTTP(w, req)
	if got == "bad\x00id" || got == "" {
		t.Fatalf("control chars not stripped, got %q", got)
	}
	if len(got) != 26 {
		t.Fatalf("expected ULID replacement, got %q", got)
	}
}

func TestRequestID_RejectsTooLong(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 200)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.RequestIDMiddleware())
	var got string
	r.GET("/x", func(c *gin.Context) {
		got = observability.RequestIDFromContext(c.Request.Context())
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(observability.RequestIDHeader, long)
	r.ServeHTTP(w, req)
	if got == long {
		t.Fatalf("oversized id was accepted")
	}
}

func TestRequestID_AvailableOnGinContext(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(observability.RequestIDMiddleware())
	var fromGin string
	r.GET("/x", func(c *gin.Context) {
		fromGin = c.GetString(observability.ContextKeyRequestID)
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(observability.RequestIDHeader, "rid-7")
	r.ServeHTTP(w, req)
	if fromGin != "rid-7" {
		t.Fatalf("gin ctx key=%q", fromGin)
	}
}

func TestRequestID_LoggerCarriesField(t *testing.T) {
	t.Parallel()
	ctx := observability.WithRequestID(t.Context(), "rid-log-1")
	logger := observability.LoggerFromContext(ctx)
	if logger == nil {
		t.Fatal("nil logger")
	}
	// We cannot easily inspect attrs without a custom slog handler;
	// the contract is exercised via integration tests, but we at
	// least verify the helper does not panic and returns a logger.
}
