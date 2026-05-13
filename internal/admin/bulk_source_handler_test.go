package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type fakeBulkRepo struct {
	mu      sync.Mutex
	failOn  map[string]error
	updated map[string]admin.SourceStatus
	removed map[string]bool
}

func (f *fakeBulkRepo) Update(_ context.Context, tenantID, id string, patch admin.UpdatePatch) (*admin.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[id]; ok {
		return nil, err
	}
	if patch.Status != nil {
		f.updated[id] = *patch.Status
	}
	return &admin.Source{ID: id, TenantID: tenantID}, nil
}

func (f *fakeBulkRepo) MarkRemoving(_ context.Context, tenantID, id string) (*admin.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[id]; ok {
		return nil, err
	}
	f.removed[id] = true
	return &admin.Source{ID: id, TenantID: tenantID}, nil
}

func newBulkRepo() *fakeBulkRepo {
	return &fakeBulkRepo{
		failOn:  map[string]error{},
		updated: map[string]admin.SourceStatus{},
		removed: map[string]bool{},
	}
}

func TestBulkSourceHandler_PauseMixedResults(t *testing.T) {
	repo := newBulkRepo()
	repo.failOn["s2"] = errors.New("not found")
	audr := &recordingAudit{}
	h, err := admin.NewBulkSourceHandler(repo, audr)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "ta")
		c.Set(audit.ActorContextKey, "u1")
		c.Next()
	})
	h.Register(r.Group("/"))
	body := `{"action":"pause","source_ids":["s1","s2","s3"]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.BulkSourceResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OK != 2 || resp.Failed != 1 {
		t.Fatalf("expected 2 ok / 1 failed; got %+v", resp)
	}
	if repo.updated["s1"] != admin.SourceStatusPaused || repo.updated["s3"] != admin.SourceStatusPaused {
		t.Fatalf("expected pause status on s1, s3; got %v", repo.updated)
	}
	// audit fired only for successes
	if len(audr.logs) != 2 {
		t.Fatalf("expected 2 audit rows; got %d", len(audr.logs))
	}
}

func TestBulkSourceHandler_DisconnectAndResume(t *testing.T) {
	repo := newBulkRepo()
	h, _ := admin.NewBulkSourceHandler(repo, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	body := `{"action":"disconnect","source_ids":["a","b"]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if !repo.removed["a"] || !repo.removed["b"] {
		t.Fatalf("expected both marked removing")
	}
	// resume
	body = `{"action":"resume","source_ids":["c"]}`
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || repo.updated["c"] != admin.SourceStatusActive {
		t.Fatalf("expected resume on c; got %v", repo.updated)
	}
}

func TestBulkSourceHandler_InvalidAction(t *testing.T) {
	repo := newBulkRepo()
	h, _ := admin.NewBulkSourceHandler(repo, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	body := `{"action":"delete","source_ids":["a"]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", w.Code)
	}
}

func TestBulkSourceHandler_EmptyOrTooMany(t *testing.T) {
	repo := newBulkRepo()
	h, _ := admin.NewBulkSourceHandler(repo, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	// empty source_ids
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(`{"action":"pause","source_ids":[]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty expected 400; got %d", w.Code)
	}
}

// -------------------------------------------------------------
// Round-19 Task 25 — reindex + rotate-credentials surfaces.
// -------------------------------------------------------------

type fakeBulkReindexer struct {
	mu   sync.Mutex
	hits []string
	err  error
}

func (f *fakeBulkReindexer) EnqueueReindex(_ context.Context, _, sourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, sourceID)

	return f.err
}

type fakeBulkRotator struct {
	mu   sync.Mutex
	hits []string
	err  error
}

func (f *fakeBulkRotator) RotateCredentials(_ context.Context, _, sourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, sourceID)

	return f.err
}

func TestBulkSourceHandler_Reindex_Round19(t *testing.T) {
	repo := newBulkRepo()
	audr := &recordingAudit{}
	reindexer := &fakeBulkReindexer{}
	h, _ := admin.NewBulkSourceHandler(repo, audr)
	h.WithReindexer(reindexer)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	body := `{"action":"reindex","source_ids":["s1","s2"]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if len(reindexer.hits) != 2 {
		t.Fatalf("expected 2 reindex hits, got %v", reindexer.hits)
	}
	if len(audr.logs) != 2 {
		t.Fatalf("expected 2 audit rows, got %d", len(audr.logs))
	}
	for _, log := range audr.logs {
		if log.Action != audit.ActionReindexRequested {
			t.Fatalf("expected ActionReindexRequested, got %q", log.Action)
		}
	}
}

func TestBulkSourceHandler_RotateCredentials_Round19(t *testing.T) {
	repo := newBulkRepo()
	audr := &recordingAudit{}
	rotator := &fakeBulkRotator{}
	h, _ := admin.NewBulkSourceHandler(repo, audr)
	h.WithCredRotator(rotator)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	body := `{"action":"rotate-credentials","source_ids":["s1"]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if len(rotator.hits) != 1 {
		t.Fatalf("expected 1 rotate hit, got %v", rotator.hits)
	}
	if len(audr.logs) != 1 || audr.logs[0].Action != audit.ActionSourceCredentialsRotated {
		t.Fatalf("audit expected ActionSourceCredentialsRotated, got %+v", audr.logs)
	}
}

func TestBulkSourceHandler_ReindexUnconfiguredFails(t *testing.T) {
	repo := newBulkRepo()
	h, _ := admin.NewBulkSourceHandler(repo, nil)
	// no .WithReindexer called
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk", strings.NewReader(`{"action":"reindex","source_ids":["s1"]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured reindex expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestBulkSourceHandler_BulkReindexAlias — Round-24 Task 13.
//
// Asserts that POST /v1/admin/sources/bulk-reindex (the alias
// route) walks the same pipeline as the generic /bulk endpoint
// with action=reindex.
func TestBulkSourceHandler_BulkReindexAlias(t *testing.T) {
	repo := newBulkRepo()
	audr := &recordingAudit{}
	h, _ := admin.NewBulkSourceHandler(repo, audr)
	reindexer := &fakeBulkReindexer{}
	h.WithReindexer(reindexer)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "ta")
		c.Set(audit.ActorContextKey, "u1")
		c.Next()
	})
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk-reindex", strings.NewReader(`{"source_ids":["s1","s2"]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if len(reindexer.hits) != 2 {
		t.Fatalf("expected 2 reindex hits, got %v", reindexer.hits)
	}
}

// TestBulkSourceHandler_BulkRotateAlias — Round-24 Task 13.
func TestBulkSourceHandler_BulkRotateAlias(t *testing.T) {
	repo := newBulkRepo()
	audr := &recordingAudit{}
	h, _ := admin.NewBulkSourceHandler(repo, audr)
	rotator := &fakeBulkRotator{}
	h.WithCredRotator(rotator)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "ta")
		c.Set(audit.ActorContextKey, "u1")
		c.Next()
	})
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/bulk-rotate", strings.NewReader(`{"source_ids":["s1"]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if len(rotator.hits) != 1 {
		t.Fatalf("expected 1 rotate hit, got %v", rotator.hits)
	}
}
