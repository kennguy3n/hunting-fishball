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

// TestRound9_AdminCatalogEntries — Task 12. Every new admin code
// is registered in the catalog with the expected HTTP status.
func TestRound9_AdminCatalogEntries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code   errcat.Code
		status int
	}{
		{errcat.CodeCacheWarmFailed, http.StatusInternalServerError},
		{errcat.CodeBudgetInvalid, http.StatusBadRequest},
		{errcat.CodeBudgetLookupFailed, http.StatusInternalServerError},
		{errcat.CodeCacheConfigFailed, http.StatusInternalServerError},
		{errcat.CodeSyncHistoryFailed, http.StatusInternalServerError},
		{errcat.CodePinnedResultsFailed, http.StatusInternalServerError},
		{errcat.CodePipelineHealthFailed, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.code), func(t *testing.T) {
			t.Parallel()
			entry, ok := errcat.DefaultCatalog[tc.code]
			if !ok {
				t.Fatalf("code %s missing from DefaultCatalog", tc.code)
			}
			if entry.HTTPStatus != tc.status {
				t.Fatalf("code %s: status = %d, want %d", tc.code, entry.HTTPStatus, tc.status)
			}
			if entry.Message == "" {
				t.Fatalf("code %s: empty message", tc.code)
			}
		})
	}
}

// TestRound9_AdminCodes_RoundTripThroughMiddleware confirms each
// new code emits the expected status + envelope through the gin
// error middleware.
func TestRound9_AdminCodes_RoundTripThroughMiddleware(t *testing.T) {
	t.Parallel()
	codes := []errcat.Code{
		errcat.CodeCacheWarmFailed,
		errcat.CodeBudgetInvalid,
		errcat.CodeBudgetLookupFailed,
		errcat.CodeCacheConfigFailed,
		errcat.CodeSyncHistoryFailed,
		errcat.CodePinnedResultsFailed,
		errcat.CodePipelineHealthFailed,
	}
	for _, code := range codes {
		code := code
		t.Run(string(code), func(t *testing.T) {
			t.Parallel()
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(errcat.Middleware())
			r.GET("/x", func(c *gin.Context) {
				_ = c.Error(errcat.New(code, "boom"))
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.ServeHTTP(w, req)

			want := errcat.DefaultCatalog[code].HTTPStatus
			if w.Code != want {
				t.Fatalf("status = %d, want %d", w.Code, want)
			}
			var got errcat.Response
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Error.Code != code {
				t.Fatalf("code = %s, want %s", got.Error.Code, code)
			}
		})
	}
}

// TestCatalog_AllCodesUniqueAndValid — Round-11 Task 13.
//
// Asserts that every Code in DefaultCatalog has a valid HTTP
// status (between 400 and 599 inclusive, the operator surface)
// and that no two codes map to the same string literal. The
// uniqueness side is the important one: a typo that duplicates
// an existing code value silently overwrites an existing entry
// and breaks the alert pipeline downstream.
func TestCatalog_AllCodesUniqueAndValid(t *testing.T) {
	t.Parallel()
	seen := map[errcat.Code]struct{}{}
	for code, entry := range errcat.DefaultCatalog {
		if _, dup := seen[code]; dup {
			t.Errorf("duplicate Code %q in DefaultCatalog", code)
			continue
		}
		seen[code] = struct{}{}
		if code == "" {
			t.Errorf("empty Code value")
		}
		if entry.HTTPStatus < 400 || entry.HTTPStatus > 599 {
			t.Errorf("code %s: invalid HTTP status %d (must be 4xx or 5xx)", code, entry.HTTPStatus)
		}
		if entry.Message == "" {
			t.Errorf("code %s: empty Message", code)
		}
	}
	// Spot-check the Round-11 additions exist so a refactor
	// can't silently drop them.
	for _, code := range []errcat.Code{
		errcat.CodeMissingTenant,
		errcat.CodeInvalidRequestBody,
		errcat.CodeChunkQualityFailed,
		errcat.CodePolicyHistoryFailed,
		errcat.CodeABTestResultsFailed,
		errcat.CodeDashboardListFailed,
		errcat.CodeSimulatorPersistFail,
		errcat.CodeSimulatorEvalFail,
		errcat.CodeSimulatorDraftMissing,
		errcat.CodeSyncStreamFailed,
	} {
		if _, ok := errcat.DefaultCatalog[code]; !ok {
			t.Errorf("required Round-11 code missing from catalog: %s", code)
		}
	}
}
