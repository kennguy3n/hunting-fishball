package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type fakeReindexRunner struct {
	gotReq pipeline.ReindexRequest
	res    pipeline.ReindexResult
	err    error
}

func (f *fakeReindexRunner) Reindex(_ context.Context, req pipeline.ReindexRequest) (pipeline.ReindexResult, error) {
	f.gotReq = req
	if f.err != nil {
		return pipeline.ReindexResult{}, f.err
	}
	return f.res, nil
}

func setupReindexRouter(t *testing.T, runner admin.ReindexRunner, au admin.AuditWriter, tenantID, actorID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewReindexHandler(admin.ReindexHandlerConfig{Runner: runner, Audit: au})
	if err != nil {
		t.Fatalf("NewReindexHandler: %v", err)
	}
	r := gin.New()
	rg := r.Group("/", func(c *gin.Context) {
		if tenantID != "" {
			c.Set(audit.TenantContextKey, tenantID)
		}
		if actorID != "" {
			c.Set(audit.ActorContextKey, actorID)
		}
		c.Next()
	})
	h.Register(rg)
	return r
}

func TestReindexHandler_Validation(t *testing.T) {
	t.Parallel()
	if _, err := admin.NewReindexHandler(admin.ReindexHandlerConfig{}); err == nil {
		t.Fatalf("expected error for nil runner")
	}
}

func TestReindexHandler_HappyPath(t *testing.T) {
	t.Parallel()
	runner := &fakeReindexRunner{res: pipeline.ReindexResult{DocumentsEnumerated: 4, EventsEmitted: 4}}
	au := &recordAudit{}
	r := setupReindexRouter(t, runner, au, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/reindex", strings.NewReader(`{"source_id":"src-1"}`)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got admin.ReindexAdminResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.EventsEmitted != 4 {
		t.Fatalf("events=%d", got.EventsEmitted)
	}
	if runner.gotReq.TenantID != "tenant-a" || runner.gotReq.SourceID != "src-1" {
		t.Fatalf("unexpected runner request: %+v", runner.gotReq)
	}
	if len(au.calls) != 1 || au.calls[0].Action != audit.ActionReindexRequested {
		t.Fatalf("audit log not recorded: %+v", au.calls)
	}
}

func TestReindexHandler_RequiresTenant(t *testing.T) {
	t.Parallel()
	r := setupReindexRouter(t, &fakeReindexRunner{}, &recordAudit{}, "", "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/reindex", strings.NewReader(`{"source_id":"src-1"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestReindexHandler_BadJSON(t *testing.T) {
	t.Parallel()
	r := setupReindexRouter(t, &fakeReindexRunner{}, &recordAudit{}, "tenant-a", "u")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/reindex", strings.NewReader(`{bad`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestReindexHandler_MissingSourceID(t *testing.T) {
	t.Parallel()
	r := setupReindexRouter(t, &fakeReindexRunner{}, &recordAudit{}, "tenant-a", "u")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/reindex", strings.NewReader(`{}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestReindexHandler_RunnerError500(t *testing.T) {
	t.Parallel()
	runner := &fakeReindexRunner{err: errors.New("boom")}
	r := setupReindexRouter(t, runner, &recordAudit{}, "tenant-a", "u")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/reindex", strings.NewReader(`{"source_id":"s"}`)))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body: %s", w.Code, w.Body.String())
	}
}
