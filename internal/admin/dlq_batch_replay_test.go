package admin_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func setupBatchReplayRouter(t *testing.T, store *memDLQ, au admin.AuditWriter, replayer admin.DLQReplayer, tenantID string) *gin.Engine {
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
		c.Next()
	})
	h.Register(rg)
	h.RegisterBatchReplay(rg)
	return r
}

func postBatchReplay(t *testing.T, r *gin.Engine, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/dlq/replay", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDLQBatchReplay_HappyPath(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	now := time.Now().UTC()
	store.seed(
		&pipeline.DLQMessage{ID: "01HZZ001", TenantID: "tenant-a", SourceID: "src-1", OriginalTopic: "ingest", CreatedAt: now, ErrorText: "boom"},
		&pipeline.DLQMessage{ID: "01HZZ002", TenantID: "tenant-a", SourceID: "src-1", OriginalTopic: "ingest", CreatedAt: now, ErrorText: "boom"},
		&pipeline.DLQMessage{ID: "01HZZ003", TenantID: "tenant-a", SourceID: "src-2", OriginalTopic: "ingest", CreatedAt: now, ErrorText: "other"},
	)
	au := &recordAudit{}
	r := setupBatchReplayRouter(t, store, au, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{SourceID: "src-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.DLQBatchReplayResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Replayed != 2 || resp.Skipped != 0 || resp.Considered != 2 {
		t.Fatalf("counts: replayed=%d skipped=%d considered=%d", resp.Replayed, resp.Skipped, resp.Considered)
	}
	// Audit captured the batch event.
	if len(au.calls) != 1 || au.calls[0].Action != audit.ActionDLQReplayedBatch {
		t.Fatalf("audit: %+v", au.calls)
	}
}

func TestDLQBatchReplay_FilterByTimeRange(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	t1 := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t1.Add(48 * time.Hour)
	store.seed(
		&pipeline.DLQMessage{ID: "01HZZ010", TenantID: "tenant-a", SourceID: "s", OriginalTopic: "ingest", CreatedAt: t1, ErrorText: "x"},
		&pipeline.DLQMessage{ID: "01HZZ011", TenantID: "tenant-a", SourceID: "s", OriginalTopic: "ingest", CreatedAt: t2, ErrorText: "x"},
		&pipeline.DLQMessage{ID: "01HZZ012", TenantID: "tenant-a", SourceID: "s", OriginalTopic: "ingest", CreatedAt: t3, ErrorText: "x"},
	)
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{
		MinCreatedAt: t1.Format(time.RFC3339),
		MaxCreatedAt: t1.Add(2 * time.Hour).Format(time.RFC3339),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp admin.DLQBatchReplayResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Replayed != 2 || resp.Considered != 2 {
		t.Fatalf("expected 2 replayed, got: %+v", resp)
	}
}

func TestDLQBatchReplay_ErrorContainsCaseInsensitive(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(
		&pipeline.DLQMessage{ID: "01HZZ100", TenantID: "tenant-a", OriginalTopic: "ingest", ErrorText: "Connection RESET"},
		&pipeline.DLQMessage{ID: "01HZZ101", TenantID: "tenant-a", OriginalTopic: "ingest", ErrorText: "deadline exceeded"},
	)
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{ErrorContains: "reset"})
	var resp admin.DLQBatchReplayResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Replayed != 1 || len(resp.Outcomes) != 1 || resp.Outcomes[0].ID != "01HZZ100" {
		t.Fatalf("filter mismatch: %+v", resp)
	}
}

func TestDLQBatchReplay_MaxAttemptsSurfaceAsSkipped(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(
		&pipeline.DLQMessage{ID: "01HZZ200", TenantID: "tenant-a", OriginalTopic: "ingest", AttemptCount: 99, ErrorText: "x"},
	)
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{})
	var resp admin.DLQBatchReplayResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Skipped != 1 || resp.Replayed != 0 || resp.Outcomes[0].Reason != "max_attempts" {
		t.Fatalf("expected max_attempts skip, got: %+v", resp)
	}
}

func TestDLQBatchReplay_AlreadyReplayedSkipped(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	now := time.Now().UTC()
	store.seed(
		&pipeline.DLQMessage{ID: "01HZZ300", TenantID: "tenant-a", OriginalTopic: "ingest", ReplayedAt: &now, ErrorText: "x"},
	)
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{IncludeReplayed: true})
	var resp admin.DLQBatchReplayResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Skipped != 1 || resp.Outcomes[0].Reason != "already_replayed" {
		t.Fatalf("expected already_replayed skip: %+v", resp)
	}
}

func TestDLQBatchReplay_ForceOverridesAlreadyReplayed(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	now := time.Now().UTC()
	store.seed(
		&pipeline.DLQMessage{ID: "01HZZ400", TenantID: "tenant-a", OriginalTopic: "ingest", ReplayedAt: &now, ErrorText: "x"},
	)
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{Force: true})
	var resp admin.DLQBatchReplayResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Replayed != 1 {
		t.Fatalf("force should replay despite prior replay: %+v", resp)
	}
}

func TestDLQBatchReplay_BatchCap(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	for i := 0; i < admin.MaxReplayBatchSize+25; i++ {
		// IDs must compare lexicographically; pad with leading zeros.
		id := "01HZZ" + leftPadInt(i, 6)
		store.seed(&pipeline.DLQMessage{ID: id, TenantID: "tenant-a", OriginalTopic: "ingest", ErrorText: "x"})
	}
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp admin.DLQBatchReplayResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Considered != admin.MaxReplayBatchSize {
		t.Fatalf("expected hard cap at %d considered, got %d", admin.MaxReplayBatchSize, resp.Considered)
	}
}

func TestDLQBatchReplay_BadTimestamp(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(&pipeline.DLQMessage{ID: "01HZZ900", TenantID: "tenant-a", OriginalTopic: "ingest", ErrorText: "x"})
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{MinCreatedAt: "yesterday"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestDLQBatchReplay_TenantIsolation(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	store.seed(
		&pipeline.DLQMessage{ID: "01HZZ500", TenantID: "tenant-b", OriginalTopic: "ingest", ErrorText: "x"},
	)
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{})
	var resp admin.DLQBatchReplayResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Considered != 0 || resp.Replayed != 0 {
		t.Fatalf("cross-tenant leakage: %+v", resp)
	}
}

func TestDLQBatchReplay_MissingTenant(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	r := setupBatchReplayRouter(t, store, &recordAudit{}, store, "")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestDLQBatchReplay_NoReplayerReturns503(t *testing.T) {
	t.Parallel()
	store := newMemDLQ()
	r := setupBatchReplayRouter(t, store, &recordAudit{}, nil, "tenant-a")
	w := postBatchReplay(t, r, admin.DLQBatchReplayRequest{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", w.Code)
	}
}

func leftPadInt(i int, width int) string {
	s := ""
	for n := i; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	if s == "" {
		s = "0"
	}
	for len(s) < width {
		s = "0" + s
	}
	return s
}
