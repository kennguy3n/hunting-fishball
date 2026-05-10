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
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// sqliteExportSchema mirrors migrations/015_export_jobs.sql with
// SQLite-friendly types (TEXT instead of VARCHAR/TIMESTAMPTZ).
const sqliteExportSchema = `
CREATE TABLE export_jobs (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    actor_id     TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',
    download_url TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL,
    started_at   DATETIME,
    completed_at DATETIME
);
`

func newSQLiteExportRepo(t *testing.T) *admin.ExportJobRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteExportSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return admin.NewExportJobRepository(db)
}

func TestExportRepository_Lifecycle(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	ctx := context.Background()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	job := &admin.ExportJob{TenantID: "tenant-a", ActorID: "user-1"}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if job.ID == "" {
		t.Fatalf("Create did not assign an ID")
	}
	if job.Status != admin.ExportJobStatusPending {
		t.Fatalf("default status: %q", job.Status)
	}

	got, err := repo.Get(ctx, "tenant-a", job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActorID != "user-1" {
		t.Fatalf("ActorID: %q", got.ActorID)
	}

	if err := repo.MarkRunning(ctx, job.ID, now); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := repo.MarkRunning(ctx, job.ID, now); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("MarkRunning second call: %v", err)
	}
	if err := repo.MarkSucceeded(ctx, job.ID, "https://x/archive.json", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}
	got, _ = repo.Get(ctx, "tenant-a", job.ID)
	if got.Status != admin.ExportJobStatusSucceeded {
		t.Fatalf("Status: %q", got.Status)
	}
	if got.DownloadURL != "https://x/archive.json" {
		t.Fatalf("DownloadURL: %q", got.DownloadURL)
	}
}

func TestExportRepository_TenantIsolation(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	ctx := context.Background()

	job := &admin.ExportJob{TenantID: "tenant-a"}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.Get(ctx, "tenant-b", job.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("cross-tenant Get returned: %v", err)
	}
}

func TestExportRepository_ClaimNextPending(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	older := &admin.ExportJob{TenantID: "tenant-a"}
	older.CreatedAt = base
	if err := repo.Create(ctx, older); err != nil {
		t.Fatalf("Create older: %v", err)
	}
	newer := &admin.ExportJob{TenantID: "tenant-b"}
	newer.CreatedAt = base.Add(time.Hour)
	if err := repo.Create(ctx, newer); err != nil {
		t.Fatalf("Create newer: %v", err)
	}

	first, err := repo.ClaimNextPending(ctx, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("ClaimNextPending: %v", err)
	}
	if first.ID != older.ID {
		t.Fatalf("expected older job, got %q", first.ID)
	}
	if first.Status != admin.ExportJobStatusRunning {
		t.Fatalf("Status: %q", first.Status)
	}

	second, err := repo.ClaimNextPending(ctx, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("ClaimNextPending second: %v", err)
	}
	if second.ID != newer.ID {
		t.Fatalf("expected newer job, got %q", second.ID)
	}

	if _, err := repo.ClaimNextPending(ctx, base.Add(2*time.Hour)); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("empty queue: %v", err)
	}
}

// fakeSourceLister returns a fixed slice for ListAllForTenant.
type fakeSourceLister struct {
	rows []admin.Source
	err  error
}

func (f fakeSourceLister) ListAllForTenant(_ context.Context, tenantID string) ([]admin.Source, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]admin.Source, 0, len(f.rows))
	for _, r := range f.rows {
		if r.TenantID == tenantID {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeAuditLister returns a filtered slice.
type fakeAuditLister struct {
	rows []audit.AuditLog
	err  error
}

func (f fakeAuditLister) ListForExport(_ context.Context, tenantID string, since time.Time) ([]audit.AuditLog, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]audit.AuditLog, 0, len(f.rows))
	for _, r := range f.rows {
		if r.TenantID == tenantID && !r.CreatedAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// recordingSink captures the bytes the worker would have written.
type recordingSink struct {
	mu   sync.Mutex
	data []byte
	url  string
	err  error
}

func (s *recordingSink) Write(_ context.Context, _, jobID string, payload []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.data = append([]byte(nil), payload...)
	s.url = "https://archives.example/" + jobID + ".json"
	return s.url, nil
}

// recordingAudit captures audit events.
type recordingAudit struct {
	mu   sync.Mutex
	logs []audit.AuditLog
}

func (a *recordingAudit) Create(_ context.Context, log *audit.AuditLog) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, *log)
	return nil
}

func TestExportWorker_ProcessOne_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	ctx := context.Background()
	now := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	src := admin.NewSource("tenant-a", "google-drive", admin.JSONMap{"site": "x"}, []string{"folder-1"})
	auditRow := audit.AuditLog{
		ID:           "01H000000000000000000AUDIT",
		TenantID:     "tenant-a",
		Action:       audit.ActionSourceConnected,
		ResourceType: "source",
		ResourceID:   src.ID,
		CreatedAt:    now.Add(-24 * time.Hour),
	}

	sink := &recordingSink{}
	auditW := &recordingAudit{}
	w, err := admin.NewExportWorker(admin.ExportWorkerConfig{
		Repo:    repo,
		Sources: fakeSourceLister{rows: []admin.Source{*src}},
		Audits:  fakeAuditLister{rows: []audit.AuditLog{auditRow}},
		Sink:    sink,
		Audit:   auditW,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewExportWorker: %v", err)
	}

	job := &admin.ExportJob{TenantID: "tenant-a", ActorID: "admin-1"}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := w.ProcessOne(ctx); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}

	got, _ := repo.Get(ctx, "tenant-a", job.ID)
	if got.Status != admin.ExportJobStatusSucceeded {
		t.Fatalf("Status: %q", got.Status)
	}
	if !strings.HasPrefix(got.DownloadURL, "https://archives.example/") {
		t.Fatalf("DownloadURL: %q", got.DownloadURL)
	}
	if len(sink.data) == 0 {
		t.Fatalf("sink received no bytes")
	}

	var arc admin.ExportArchive
	if err := json.Unmarshal(sink.data, &arc); err != nil {
		t.Fatalf("unmarshal archive: %v", err)
	}
	if arc.TenantID != "tenant-a" {
		t.Fatalf("archive TenantID: %q", arc.TenantID)
	}
	if len(arc.Sources) != 1 || arc.Sources[0].ID != src.ID {
		t.Fatalf("archive Sources: %+v", arc.Sources)
	}
	if len(arc.AuditLogs) != 1 || arc.AuditLogs[0].ID != auditRow.ID {
		t.Fatalf("archive AuditLogs: %+v", arc.AuditLogs)
	}

	// Audit recorder should have one completion event.
	gotAudit := false
	for _, l := range auditW.logs {
		if l.Action == audit.ActionTenantExportCompleted {
			gotAudit = true
		}
	}
	if !gotAudit {
		t.Fatalf("expected ActionTenantExportCompleted, got: %+v", auditW.logs)
	}
}

func TestExportWorker_ProcessOne_FailsOnSink(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	ctx := context.Background()

	sink := &recordingSink{err: errors.New("disk full")}
	w, err := admin.NewExportWorker(admin.ExportWorkerConfig{
		Repo:    repo,
		Sources: fakeSourceLister{},
		Sink:    sink,
		Now:     func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("NewExportWorker: %v", err)
	}
	job := &admin.ExportJob{TenantID: "tenant-a"}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = w.ProcessOne(ctx)
	if err == nil {
		t.Fatalf("expected error")
	}
	got, _ := repo.Get(ctx, "tenant-a", job.ID)
	if got.Status != admin.ExportJobStatusFailed {
		t.Fatalf("Status after failure: %q", got.Status)
	}
	if !strings.Contains(got.Error, "disk full") {
		t.Fatalf("Error message: %q", got.Error)
	}
}

func TestExportWorker_ProcessOne_EmptyQueue(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	w, err := admin.NewExportWorker(admin.ExportWorkerConfig{
		Repo: repo,
		Sink: &recordingSink{},
	})
	if err != nil {
		t.Fatalf("NewExportWorker: %v", err)
	}
	if err := w.ProcessOne(context.Background()); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound, got %v", err)
	}
}

func newDataExportRouter(t *testing.T, repo *admin.ExportJobRepository, audit *recordingAudit, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenant_id", tenantID)
		c.Set("actor_id", "admin-1")
		c.Next()
	})
	h, err := admin.NewDataExportHandler(admin.DataExportHandlerConfig{
		Repo:  repo,
		Audit: audit,
	})
	if err != nil {
		t.Fatalf("NewDataExportHandler: %v", err)
	}
	rg := &r.RouterGroup
	h.Register(rg)
	return r
}

func TestDataExportHandler_CreateAndGet(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	auditW := &recordingAudit{}
	r := newDataExportRouter(t, repo, auditW, "tenant-a")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/tenant-a/export", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status: %d body=%s", w.Code, w.Body.String())
	}
	var created admin.ExportJob
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Status != admin.ExportJobStatusPending {
		t.Fatalf("Status: %q", created.Status)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/tenant-a/export/"+created.ID, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status: %d body=%s", w.Code, w.Body.String())
	}

	if len(auditW.logs) != 1 || auditW.logs[0].Action != audit.ActionTenantExportRequested {
		t.Fatalf("audit log: %+v", auditW.logs)
	}
}

func TestDataExportHandler_TenantMismatch(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	r := newDataExportRouter(t, repo, &recordingAudit{}, "tenant-a")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/tenant-b/export", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestDataExportHandler_GetMissing(t *testing.T) {
	t.Parallel()
	repo := newSQLiteExportRepo(t)
	r := newDataExportRouter(t, repo, &recordingAudit{}, "tenant-a")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/tenant-a/export/01H00000000000000000000000", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}
