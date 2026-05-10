package errors_test

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	errcat "github.com/kennguy3n/hunting-fishball/internal/errors"
)

func TestNew_AndError(t *testing.T) {
	t.Parallel()
	e := errcat.New(errcat.CodeTenantNotFound, "no such tenant")
	if e.Code != errcat.CodeTenantNotFound {
		t.Fatalf("code = %s", e.Code)
	}
	if got := e.Error(); got != "ERR_TENANT_NOT_FOUND: no such tenant" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestWrap_PreservesCause(t *testing.T) {
	t.Parallel()
	cause := stderrors.New("boom")
	e := errcat.Wrap(errcat.CodeInternal, cause)
	if !stderrors.Is(e, cause) {
		t.Fatalf("Wrap should preserve cause for errors.Is")
	}
}

func TestResolve_FallsBackToUnknown(t *testing.T) {
	t.Parallel()
	e := errcat.New(errcat.Code("ERR_NOT_REGISTERED"), "x")
	entry := errcat.Resolve(e)
	if entry.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("unknown code should map to 500, got %d", entry.HTTPStatus)
	}
}

func TestMiddleware_TypedErrorEmitsCatalogEntry(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errcat.Middleware())
	r.GET("/x", func(c *gin.Context) {
		_ = c.Error(errcat.New(errcat.CodeTenantNotFound, "tenant-a missing"))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	var got errcat.Response
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Code != errcat.CodeTenantNotFound {
		t.Fatalf("code = %s", got.Error.Code)
	}
	if got.Error.Message != "tenant-a missing" {
		t.Fatalf("message = %q", got.Error.Message)
	}
}

func TestMiddleware_PlainErrorBecomesUnknown(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errcat.Middleware())
	r.GET("/x", func(c *gin.Context) {
		_ = c.Error(stderrors.New("raw"))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("plain error should map to 500, got %d", w.Code)
	}
	var got errcat.Response
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Code != errcat.CodeUnknown {
		t.Fatalf("code = %s, want %s", got.Error.Code, errcat.CodeUnknown)
	}
}

func TestMiddleware_NoErrorPasthrough(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errcat.Middleware())
	r.GET("/x", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestErrorWith_AddsDetails(t *testing.T) {
	t.Parallel()
	e := errcat.New(errcat.CodeBadRequest, "x").With("field", "name")
	if e.Details["field"] != "name" {
		t.Fatalf("details not set: %#v", e.Details)
	}
}
