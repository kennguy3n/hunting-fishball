package retrieval_test

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
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

const sqliteFeedbackSchema = `
CREATE TABLE feedback_events (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    query_id    TEXT NOT NULL,
    chunk_id    TEXT NOT NULL,
    user_id     TEXT NOT NULL DEFAULT '',
    relevant    BOOLEAN NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL
);
`

type feedbackAuditFake struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (f *feedbackAuditFake) Create(_ context.Context, log *audit.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if log == nil {
		return errors.New("nil log")
	}
	f.logs = append(f.logs, log)
	return nil
}

func (f *feedbackAuditFake) actions() []audit.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.Action, len(f.logs))
	for i, l := range f.logs {
		out[i] = l.Action
	}
	return out
}

func newFeedbackRig(t *testing.T, tenantID string) (*gin.Engine, *retrieval.FeedbackRepository, *feedbackAuditFake) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteFeedbackSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	repo := retrieval.NewFeedbackRepository(db)
	au := &feedbackAuditFake{}
	h, err := retrieval.NewFeedbackHandler(retrieval.FeedbackHandlerConfig{Repo: repo, Audit: au})
	if err != nil {
		t.Fatalf("NewFeedbackHandler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if tenantID != "" {
			c.Set(audit.TenantContextKey, tenantID)
			c.Set(audit.ActorContextKey, "user-1")
		}
		c.Next()
	})
	rg := r.Group("/v1")
	h.RegisterFeedback(rg)
	return r, repo, au
}

func TestFeedbackHandler_HappyPath(t *testing.T) {
	t.Parallel()
	r, repo, au := newFeedbackRig(t, "tenant-a")
	body := []byte(`{"query_id":"q1","chunk_id":"chunk-1","relevant":true,"note":"good"}`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/retrieve/feedback", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp retrieval.FeedbackResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID == "" || resp.CreatedAt.IsZero() {
		t.Fatalf("response missing id or created_at: %+v", resp)
	}
	rows, err := repo.ListByTenant(context.Background(), retrieval.FeedbackFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ChunkID != "chunk-1" || rows[0].UserID != "user-1" || !rows[0].Relevant {
		t.Fatalf("persisted row mismatch: %+v", rows)
	}
	got := au.actions()
	if len(got) != 1 || got[0] != audit.ActionRetrievalFeedback {
		t.Fatalf("audit actions: %v", got)
	}
}

func TestFeedbackHandler_NegativeSignal(t *testing.T) {
	t.Parallel()
	r, repo, _ := newFeedbackRig(t, "tenant-a")
	body := []byte(`{"query_id":"q1","chunk_id":"chunk-2","relevant":false}`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/retrieve/feedback", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rows, _ := repo.ListByTenant(context.Background(), retrieval.FeedbackFilter{TenantID: "tenant-a"})
	if len(rows) != 1 || rows[0].Relevant {
		t.Fatalf("expected single relevant=false row, got %+v", rows)
	}
}

func TestFeedbackHandler_MissingTenant401(t *testing.T) {
	t.Parallel()
	r, _, _ := newFeedbackRig(t, "")
	body := []byte(`{"query_id":"q1","chunk_id":"c","relevant":true}`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/retrieve/feedback", bytes.NewReader(body)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestFeedbackHandler_BadRequestNoRelevant(t *testing.T) {
	t.Parallel()
	r, _, _ := newFeedbackRig(t, "tenant-a")
	body := []byte(`{"query_id":"q1","chunk_id":"c"}`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/retrieve/feedback", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestFeedbackHandler_TenantIsolationOnList(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteFeedbackSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	repo := retrieval.NewFeedbackRepository(db)
	ctx := context.Background()
	_ = repo.Create(ctx, &retrieval.FeedbackEvent{TenantID: "tenant-a", QueryID: "q", ChunkID: "x", Relevant: true})
	_ = repo.Create(ctx, &retrieval.FeedbackEvent{TenantID: "tenant-b", QueryID: "q", ChunkID: "x", Relevant: true})
	rows, err := repo.ListByTenant(ctx, retrieval.FeedbackFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TenantID != "tenant-a" {
		t.Fatalf("tenant isolation broken: %+v", rows)
	}
}

func TestFeedbackHandler_FilterByQueryAndChunk(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	db, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	_ = db.Exec(sqliteFeedbackSchema).Error
	repo := retrieval.NewFeedbackRepository(db)
	ctx := context.Background()
	_ = repo.Create(ctx, &retrieval.FeedbackEvent{TenantID: "tenant-a", QueryID: "q1", ChunkID: "x", Relevant: true})
	_ = repo.Create(ctx, &retrieval.FeedbackEvent{TenantID: "tenant-a", QueryID: "q1", ChunkID: "y", Relevant: true})
	_ = repo.Create(ctx, &retrieval.FeedbackEvent{TenantID: "tenant-a", QueryID: "q2", ChunkID: "x", Relevant: true})
	byQ, _ := repo.ListByTenant(ctx, retrieval.FeedbackFilter{TenantID: "tenant-a", QueryID: "q1"})
	if len(byQ) != 2 {
		t.Fatalf("query filter: got %d", len(byQ))
	}
	byC, _ := repo.ListByTenant(ctx, retrieval.FeedbackFilter{TenantID: "tenant-a", ChunkID: "x"})
	if len(byC) != 2 {
		t.Fatalf("chunk filter: got %d", len(byC))
	}
}
