package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
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

// List mirrors *pipeline.DLQStoreGORM.List: it filters by tenant +
// optional original_topic + replayed flag, sorts by id DESC,
// applies the page_token cursor, and returns at most
// EffectiveDLQPageSize(f.PageSize)+1 rows. The +1 is the standard
// fetch-N+1 probe the admin handler relies on to emit
// next_page_token only when more rows truly exist.
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
		if f.PageToken != "" && r.ID >= f.PageToken {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	limit := pipeline.EffectiveDLQPageSize(f.PageSize) + 1
	if len(out) > limit {
		out = out[:limit]
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

// TestDLQHandler_List_PaginationBoundaries pins the fetch-N+1
// next_page_token logic in the handler. Three boundaries to verify:
//
//  1. Empty result — next_page_token MUST be empty even though the
//     caller didn't pass page_size (a previous regression emitted
//     a token whenever any rows existed because the guard
//     short-circuited on filter.PageSize == 0).
//  2. Last page that ends exactly on a pageSize boundary —
//     next_page_token MUST be empty. The previous bug used
//     `len(rows) >= filter.PageSize`, so a page that filled to the
//     limit always emitted a phantom token, causing clients to
//     make at least one extra empty round-trip.
//  3. First page when more rows exist — next_page_token MUST be
//     the last item's ID and `items` MUST be trimmed back to
//     pageSize (no off-by-one leak from the fetch-N+1 probe).
func TestDLQHandler_List_PaginationBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("empty result emits no token", func(t *testing.T) {
		t.Parallel()
		store := newMemDLQ()
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
		if len(got.Items) != 0 {
			t.Fatalf("expected 0 items, got %d", len(got.Items))
		}
		if got.NextPageToken != "" {
			t.Fatalf("expected empty next_page_token, got %q", got.NextPageToken)
		}
	})

	t.Run("exactly pageSize rows emits no token", func(t *testing.T) {
		t.Parallel()
		store := newMemDLQ()
		// Seed exactly 5 rows; request page_size=5. Pre-fix this
		// emitted a phantom token; post-fix it must not.
		for i := 0; i < 5; i++ {
			store.seed(&pipeline.DLQMessage{
				ID:            fmt.Sprintf("01HZRYDLQID0000000000PAGE%02d", i),
				TenantID:      "tenant-a",
				OriginalTopic: "ingest",
			})
		}
		r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq?page_size=5", nil))
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
		if len(got.Items) != 5 {
			t.Fatalf("expected 5 items, got %d", len(got.Items))
		}
		if got.NextPageToken != "" {
			t.Fatalf("expected empty next_page_token (last page on boundary), got %q", got.NextPageToken)
		}
	})

	t.Run("more rows than pageSize trims and emits token", func(t *testing.T) {
		t.Parallel()
		store := newMemDLQ()
		// Seed 7 rows; request page_size=5. Expect items=5 and
		// next_page_token == ID of the 5th-newest row (index 4).
		for i := 0; i < 7; i++ {
			store.seed(&pipeline.DLQMessage{
				ID:            fmt.Sprintf("01HZRYDLQID0000000000PAGE%02d", i),
				TenantID:      "tenant-a",
				OriginalTopic: "ingest",
			})
		}
		r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq?page_size=5", nil))
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
		if len(got.Items) != 5 {
			t.Fatalf("expected 5 items (trimmed from fetch-N+1), got %d", len(got.Items))
		}
		want := got.Items[4].ID
		if got.NextPageToken != want {
			t.Fatalf("next_page_token=%q, want %q (last item's ID)", got.NextPageToken, want)
		}
		// Cursor-walk one more page to verify the token actually
		// returns the next batch (the remaining 2 rows) without
		// duplicates.
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq?page_size=5&page_token="+got.NextPageToken, nil))
		if w2.Code != http.StatusOK {
			t.Fatalf("page-2 status: %d body: %s", w2.Code, w2.Body.String())
		}
		var got2 struct {
			Items         []pipeline.DLQMessage `json:"items"`
			NextPageToken string                `json:"next_page_token"`
		}
		if err := json.Unmarshal(w2.Body.Bytes(), &got2); err != nil {
			t.Fatalf("decode page-2: %v", err)
		}
		if len(got2.Items) != 2 {
			t.Fatalf("expected 2 items on page-2, got %d", len(got2.Items))
		}
		if got2.NextPageToken != "" {
			t.Fatalf("expected empty next_page_token on final page, got %q", got2.NextPageToken)
		}
		// No ID overlap between the two pages.
		seen := map[string]bool{}
		for _, it := range got.Items {
			seen[it.ID] = true
		}
		for _, it := range got2.Items {
			if seen[it.ID] {
				t.Fatalf("page-2 leaked duplicate id %q from page-1", it.ID)
			}
		}
	})
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

// TestDLQHandler_List_CursorAlias asserts cursor / limit aliases
// are forwarded as PageToken / PageSize and `next_cursor` is
// emitted alongside the legacy `next_page_token`.
func TestDLQHandler_List_CursorAlias(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	// Seed 3 messages so a limit=1 query has a cursor to emit.
	store.seed(
		&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000A", TenantID: "tenant-a", OriginalTopic: "ingest"},
		&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000B", TenantID: "tenant-a", OriginalTopic: "ingest"},
		&pipeline.DLQMessage{ID: "01HZRYDLQID00000000000000C", TenantID: "tenant-a", OriginalTopic: "ingest"},
	)
	r := setupDLQRouter(t, store, &recordAudit{}, store, "tenant-a", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq?limit=1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Items         []map[string]any `json:"items"`
		NextPageToken string           `json:"next_page_token"`
		NextCursor    string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body: %s", err, w.Body.String())
	}
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	if body.NextCursor == "" || body.NextCursor != body.NextPageToken {
		t.Fatalf("next_cursor / next_page_token must mirror: %+v", body)
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
