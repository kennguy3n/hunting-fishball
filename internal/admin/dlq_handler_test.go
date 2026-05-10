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
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// memDLQ is an in-memory pipeline.DLQReader+Replayer used by the
// admin handler tests.
type memDLQ struct {
	mu       sync.Mutex
	rows     map[string]*pipeline.DLQMessage
	replays  []string
	maxRet   int
	replayFn func(ctx context.Context, tenantID, id, topic string, force bool) error
}

func newMemDLQ() *memDLQ {
	return &memDLQ{rows: map[string]*pipeline.DLQMessage{}, maxRet: 5}
}

func (s *memDLQ) seed(rows ...*pipeline.DLQMessage) {
	for _, r := range rows {
		cp := *r
		s.rows[r.ID] = &cp
	}
}

func (s *memDLQ) List(_ context.Context, f pipeline.DLQListFilter) ([]pipeline.DLQMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pipeline.DLQMessage
	for _, r := range s.rows {
		if r.TenantID != f.TenantID {
			continue
		}
		if f.OriginalTopic != "" && r.OriginalTopic != f.OriginalTopic {
			continue
		}
		if !f.IncludeReplayed && r.ReplayedAt != nil {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *memDLQ) Get(_ context.Context, tenantID, id string) (*pipeline.DLQMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, pipeline.ErrDLQNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *memDLQ) Replay(ctx context.Context, tenantID, id, topic string, force bool) error {
	if s.replayFn != nil {
		return s.replayFn(ctx, tenantID, id, topic, force)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.TenantID != tenantID {
		return pipeline.ErrDLQNotFound
	}
	if r.ReplayedAt != nil && !force {
		return pipeline.ErrAlreadyReplayed
	}
	if r.AttemptCount >= s.maxRet {
		return pipeline.ErrMaxRetriesExceeded
	}
	now := time.Now().UTC()
	r.ReplayedAt = &now
	r.AttemptCount++
	s.replays = append(s.replays, id)
	return nil
}

func (s *memDLQ) MaxAttempts() int { return s.maxRet }

// recordAudit captures audit calls. Mirrors the existing fakeAudit
// in source_handler_test.go but with a unique name so both test
// files compile together.
type recordAudit struct {
	mu    sync.Mutex
	calls []*audit.AuditLog
}

func (f *recordAudit) Create(_ context.Context, log *audit.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, log)
	return nil
}

func setupDLQRouter(t *testing.T, store *memDLQ, au admin.AuditWriter, replayer admin.DLQReplayer, tenantID, actorID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewDLQHandler(admin.DLQHandlerConfig{
		Reader:      store,
		Replayer:    replayer,
		ReplayTopic: "ingest",
		Audit:       au,
	})
	if err != nil {
		t.Fatalf("NewDLQHandler: %v", err)
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

func TestDLQHandler_NewHandler_Validation(t *testing.T) {
	t.Parallel()
	if _, err := admin.NewDLQHandler(admin.DLQHandlerConfig{}); err == nil {
		t.Fatalf("expected error for nil reader")
	}
	if _, err := admin.NewDLQHandler(admin.DLQHandlerConfig{Reader: newMemDLQ(), Replayer: newMemDLQ()}); err == nil {
		t.Fatalf("expected error for missing replay topic")
	}
}

func TestDLQHandler_List_TenantScopedAndPaginates(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(
		&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest"},
		&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000B", TenantID: "tenant-a", OriginalTopic: "ingest"},
		&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000Z", TenantID: "tenant-b", OriginalTopic: "ingest"},
	)
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got struct {
		Items         []pipeline.DLQMessage `json:"items"`
		NextPageToken string                `json:"next_page_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 items (tenant-scoped), got %d", len(got.Items))
	}
	for _, it := range got.Items {
		if it.TenantID != "tenant-a" {
			t.Fatalf("cross-tenant leak: %+v", it)
		}
	}
}

func TestDLQHandler_List_BadPageSize(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq?page_size=-1", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
}

func TestDLQHandler_List_RequiresTenant(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	r := setupDLQRouter(t, store, &recordAudit{}, store, "", "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestDLQHandler_Get_TenantScoped(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest"})
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq/01HZRYDLQID00000000000000A", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	// cross-tenant returns 404
	r2 := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-b", "user-1")
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq/01HZRYDLQID00000000000000A", nil))
	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant get, got %d", w2.Code)
	}
}

func TestDLQHandler_Replay_HappyPath_RecordsAudit(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest"})
	au := &recordAudit{}
	r := setupDLQRouter(t, store, au, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/dlq/01HZRYDLQID00000000000000A/replay", strings.NewReader(`{"force":false}`)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	if len(store.replays) != 1 {
		t.Fatalf("expected 1 replay, got %d", len(store.replays))
	}
	if len(au.calls) != 1 {
		t.Fatalf("expected 1 audit call, got %d", len(au.calls))
	}
	if au.calls[0].Action != audit.ActionDLQReplayed {
		t.Fatalf("audit action=%q", au.calls[0].Action)
	}
}

func TestDLQHandler_Replay_AlreadyReplayedConflict(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := newMemDLQ()
	store.seed(&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest", ReplayedAt: &now})
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/dlq/01HZRYDLQID00000000000000A/replay", strings.NewReader(`{}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body: %s", w.Code, w.Body.String())
	}
}

func TestDLQHandler_Replay_MaxRetriesGives422(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.maxRet = 1
	store.seed(&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest", AttemptCount: 1})
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/dlq/01HZRYDLQID00000000000000A/replay", strings.NewReader(`{"force":true}`)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body: %s", w.Code, w.Body.String())
	}
}

func TestDLQHandler_Replay_NotFound(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/dlq/missing/replay", strings.NewReader(`{}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDLQHandler_Replay_Without_Replayer_Returns503(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest"})
	gin.SetMode(gin.TestMode)
	h, err := admin.NewDLQHandler(admin.DLQHandlerConfig{Reader: store, Audit: &recordAudit{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := gin.New()
	rg := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	h.Register(rg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/dlq/01HZRYDLQID00000000000000A/replay", strings.NewReader(`{}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestDLQHandler_Replay_GenericProducerErrorBubblesAs500(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest"})
	store.replayFn = func(_ context.Context, _, _, _ string, _ bool) error { return errors.New("kafka down") }
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/dlq/01HZRYDLQID00000000000000A/replay", strings.NewReader(`{}`)))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}
