package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// fakeAudit captures admin lifecycle events so tests can assert
// audit.Action emission. Create runs the same Validate() check the
// production repository does, so an admin path that builds an
// AuditLog without an ID — as forget_worker.go did before
// devin-ai-integration[bot] flagged it — fails the test instead of
// silently passing through a slice append.
type fakeAudit struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (f *fakeAudit) Create(_ context.Context, log *audit.AuditLog) error {
	if log == nil {
		return errors.New("fakeAudit: nil log")
	}
	if err := log.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, log)
	return nil
}

func (f *fakeAudit) actions() []audit.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.Action, 0, len(f.logs))
	for _, l := range f.logs {
		out = append(out, l.Action)
	}
	return out
}

// fakeValidator implements admin.ConnectorValidator without touching
// the real connector registry.
type fakeValidator struct {
	called bool
	err    error
	gotCfg connector.ConnectorConfig
}

func (f *fakeValidator) Validate(_ context.Context, _ string, cfg connector.ConnectorConfig) error {
	f.called = true
	f.gotCfg = cfg
	return f.err
}

// fakeEvents captures EmitSourceConnected calls.
type fakeEvents struct {
	mu     sync.Mutex
	called int
}

func (f *fakeEvents) EmitSourceConnected(_ context.Context, _, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	return nil
}

// router builds a Gin engine with the admin handler mounted under a
// fake auth middleware that injects tenantID. tenantID == "" means no
// tenant context.
func router(t *testing.T, h *admin.Handler, tenantID string) *gin.Engine {
	t.Helper()
	r := gin.New()
	if tenantID != "" {
		r.Use(func(c *gin.Context) {
			c.Set(audit.TenantContextKey, tenantID)
			c.Set(audit.ActorContextKey, "actor-1")
			c.Next()
		})
	}
	rg := r.Group("/")
	h.Register(rg)
	return r
}

func mustJSON(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

func TestHandler_Connect_Happy(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ad := &fakeAudit{}
	val := &fakeValidator{}
	ev := &fakeEvents{}
	h, err := admin.NewHandler(admin.HandlerConfig{
		Repo: repo, Audit: ad, Validator: val, Events: ev,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	w := httptest.NewRecorder()
	body := mustJSON(t, admin.ConnectRequest{
		ConnectorType: "google-drive",
		Config:        map[string]any{"drive": "abc"},
		Scopes:        []string{"drive-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources", body)
	req.Header.Set("Content-Type", "application/json")
	router(t, h, "tenant-a").ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var got admin.Source
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("tenant: %q", got.TenantID)
	}
	if !val.called {
		t.Fatal("Validator must be called")
	}
	if val.gotCfg.TenantID != "tenant-a" || val.gotCfg.SourceID == "" {
		t.Fatalf("validator cfg: %+v", val.gotCfg)
	}
	actions := ad.actions()
	if len(actions) != 1 || actions[0] != audit.ActionSourceConnected {
		t.Fatalf("audit actions: %v", actions)
	}
	if ev.called != 1 {
		t.Fatalf("expected one EmitSourceConnected, got %d", ev.called)
	}
}

func TestHandler_Connect_Unauthenticated(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources", mustJSON(t, admin.ConnectRequest{ConnectorType: "x"}))
	router(t, h, "").ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestHandler_Connect_ValidationError(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	val := &fakeValidator{err: errors.New("bad creds")}
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Validator: val, Audit: &fakeAudit{}})
	w := httptest.NewRecorder()
	body := mustJSON(t, admin.ConnectRequest{ConnectorType: "google-drive"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources", body)
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", w.Code)
	}
	rows, _ := repo.List(context.Background(), admin.ListFilter{TenantID: "tenant-a"})
	if len(rows) != 0 {
		t.Fatalf("validation failure must not persist: rows=%d", len(rows))
	}
}

func TestHandler_Connect_RejectsEmptyConnectorType(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Validator: &fakeValidator{}})
	w := httptest.NewRecorder()
	body := mustJSON(t, admin.ConnectRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources", body)
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestHandler_Patch_PauseAndResume(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(context.Background(), src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ad := &fakeAudit{}
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Audit: ad, Validator: &fakeValidator{}})
	r := router(t, h, "tenant-a")

	// Pause.
	paused := admin.SourceStatusPaused
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/sources/"+src.ID, mustJSON(t, admin.PatchRequest{Status: &paused}))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pause status: %d body=%s", w.Code, w.Body.String())
	}

	// Resume.
	active := admin.SourceStatusActive
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v1/admin/sources/"+src.ID, mustJSON(t, admin.PatchRequest{Status: &active}))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resume status: %d", w.Code)
	}
	got := ad.actions()
	if len(got) != 2 || got[0] != audit.ActionSourcePaused || got[1] != audit.ActionSourceResumed {
		t.Fatalf("audit actions: %v", got)
	}
}

func TestHandler_Patch_ReScope(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	src := admin.NewSource("tenant-a", "google-drive", nil, []string{"d1"})
	if err := repo.Create(context.Background(), src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ad := &fakeAudit{}
	ev := &fakeEvents{}
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Audit: ad, Validator: &fakeValidator{}, Events: ev})

	scopes := []string{"d1", "d2"}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/sources/"+src.ID, mustJSON(t, admin.PatchRequest{Scopes: &scopes}))
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if got := ad.actions(); len(got) != 1 || got[0] != audit.ActionSourceReScoped {
		t.Fatalf("audit actions: %v", got)
	}
	if ev.called != 1 {
		t.Fatalf("re-scope must trigger a sync kick-off, got %d", ev.called)
	}
}

func TestHandler_Patch_NotFound(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Audit: &fakeAudit{}, Validator: &fakeValidator{}})

	paused := admin.SourceStatusPaused
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/sources/missing", mustJSON(t, admin.PatchRequest{Status: &paused}))
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestHandler_Patch_TenantIsolation(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	srcA := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(context.Background(), srcA); err != nil {
		t.Fatalf("Create: %v", err)
	}
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Audit: &fakeAudit{}, Validator: &fakeValidator{}})

	paused := admin.SourceStatusPaused
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/sources/"+srcA.ID, mustJSON(t, admin.PatchRequest{Status: &paused}))
	router(t, h, "tenant-b").ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant patch must 404, got %d", w.Code)
	}
}

// TestHandler_Patch_ConflictOnTerminalStatus pins the HTTP surface of
// the forget-worker race fix: a PATCH against a row already in a
// terminal status must return 409 (not 200, not 404). 200 would race
// with the worker; 404 would mislead the operator into thinking the
// row was already purged.
func TestHandler_Patch_ConflictOnTerminalStatus(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(context.Background(), src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.MarkRemoving(context.Background(), "tenant-a", src.ID); err != nil {
		t.Fatalf("MarkRemoving: %v", err)
	}
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Audit: &fakeAudit{}, Validator: &fakeValidator{}})

	active := admin.SourceStatusActive
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/sources/"+src.ID, mustJSON(t, admin.PatchRequest{Status: &active}))
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("PATCH on removing source must 409, got %d body=%s", w.Code, w.Body.String())
	}

	got, err := repo.Get(context.Background(), "tenant-a", src.ID)
	if err != nil {
		t.Fatalf("Get after rejected patch: %v", err)
	}
	if got.Status != admin.SourceStatusRemoving {
		t.Fatalf("status mutated despite 409: got %q want %q", got.Status, admin.SourceStatusRemoving)
	}
}

func TestHandler_Delete_MarksRemoving(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(context.Background(), src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ad := &fakeAudit{}
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Audit: ad, Validator: &fakeValidator{}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/sources/"+src.ID, nil)
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: %d", w.Code)
	}
	got, _ := repo.Get(context.Background(), "tenant-a", src.ID)
	if got.Status != admin.SourceStatusRemoving {
		t.Fatalf("Status: %q", got.Status)
	}
	if a := ad.actions(); len(a) != 1 || a[0] != audit.ActionSourcePurged {
		t.Fatalf("audit actions: %v", a)
	}
}

func TestHandler_List_TenantOnly(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	for _, tenant := range []string{"tenant-a", "tenant-b", "tenant-a"} {
		s := admin.NewSource(tenant, "slack", nil, nil)
		if err := repo.Create(context.Background(), s); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Validator: &fakeValidator{}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources", nil)
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var body struct {
		Items []admin.Source `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("expected 2 items for tenant-a, got %d", len(body.Items))
	}
	for _, s := range body.Items {
		if s.TenantID != "tenant-a" {
			t.Fatalf("tenant leak in list: %+v", s)
		}
	}
}

// fakeHealth returns a fixed Health row for tests.
type fakeHealth struct {
	row *admin.Health
}

func (f *fakeHealth) Get(_ context.Context, _, _ string) (*admin.Health, error) { return f.row, nil }

func TestHandler_Health_ReturnsRow(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(context.Background(), src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	h, _ := admin.NewHandler(admin.HandlerConfig{
		Repo:      repo,
		Validator: &fakeValidator{},
		Health: &fakeHealth{row: &admin.Health{
			TenantID: "tenant-a", SourceID: src.ID,
			Status: admin.HealthStatusHealthy,
		}},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/"+src.ID+"/health", nil)
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var body admin.Health
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Status != admin.HealthStatusHealthy {
		t.Fatalf("Status: %q", body.Status)
	}
}
