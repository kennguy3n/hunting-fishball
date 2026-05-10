package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// sqliteTenantSchema mirrors migrations/008_tenant_status.sql in
// SQLite-compatible types so the deleter unit tests don't need
// Postgres.
const sqliteTenantSchema = `
CREATE TABLE tenants (
    tenant_id     TEXT PRIMARY KEY,
    tenant_status TEXT NOT NULL DEFAULT 'active',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

func newTenantStatusRepo(t *testing.T) (*admin.TenantStatusRepoGORM, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec(sqliteTenantSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return admin.NewTenantStatusRepoGORM(db), db
}

// recordingTenantSweeper records each ForgetTenant call so tests
// can assert ordering / idempotency. Distinct from the per-source
// recordingSweeper in forget_worker_test.go.
type recordingTenantSweeper struct {
	calls atomic.Int64
	mu    sync.Mutex
	seen  []string
	err   error
}

func (r *recordingTenantSweeper) ForgetTenant(_ context.Context, tenantID string) error {
	r.calls.Add(1)
	r.mu.Lock()
	r.seen = append(r.seen, tenantID)
	r.mu.Unlock()
	return r.err
}

type recordingTenantAudit struct {
	mu      sync.Mutex
	actions []audit.Action
}

func (r *recordingTenantAudit) Create(_ context.Context, log *audit.AuditLog) error {
	r.mu.Lock()
	r.actions = append(r.actions, log.Action)
	r.mu.Unlock()
	return nil
}

func TestTenantStatusRepoGORM_GetStatusDefaultsActive(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	got, err := repo.GetStatus(context.Background(), "tenant-z")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if got != admin.TenantStatusActive {
		t.Fatalf("got %q want %q", got, admin.TenantStatusActive)
	}
}

func TestTenantStatusRepoGORM_UpsertRoundTrips(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	ctx := context.Background()
	if err := repo.UpsertStatus(ctx, "tenant-a", admin.TenantStatusPendingDeletion); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	got, err := repo.GetStatus(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if got != admin.TenantStatusPendingDeletion {
		t.Fatalf("got %q want pending_deletion", got)
	}
	// Second upsert should overwrite, not insert a duplicate.
	if err := repo.UpsertStatus(ctx, "tenant-a", admin.TenantStatusDeleted); err != nil {
		t.Fatalf("UpsertStatus 2: %v", err)
	}
	got, err = repo.GetStatus(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if got != admin.TenantStatusDeleted {
		t.Fatalf("got %q want deleted", got)
	}
}

func TestTenantDeleter_RunsFullWorkflow(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	sweeper := &recordingTenantSweeper{}
	auditor := &recordingTenantAudit{}
	d, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{
		Status:   repo,
		Sweepers: []admin.TenantSweeper{sweeper, sweeper},
		Audit:    auditor,
	})
	if err != nil {
		t.Fatalf("NewTenantDeleter: %v", err)
	}
	if err := d.Delete(context.Background(), "tenant-a", "admin-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sweeper.calls.Load() != 2 {
		t.Fatalf("sweeper calls=%d want 2", sweeper.calls.Load())
	}
	got, err := repo.GetStatus(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if got != admin.TenantStatusDeleted {
		t.Fatalf("final status=%q want deleted", got)
	}
	if len(auditor.actions) != 2 {
		t.Fatalf("audit actions=%v want 2 events", auditor.actions)
	}
	if string(auditor.actions[0]) != "tenant.deletion_requested" {
		t.Errorf("audit[0]=%q want tenant.deletion_requested", auditor.actions[0])
	}
	if string(auditor.actions[1]) != "tenant.deleted" {
		t.Errorf("audit[1]=%q want tenant.deleted", auditor.actions[1])
	}
}

func TestTenantDeleter_SweeperFailureLeavesPending(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	failing := &recordingTenantSweeper{err: errors.New("qdrant down")}
	d, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{
		Status:   repo,
		Sweepers: []admin.TenantSweeper{failing},
	})
	if err != nil {
		t.Fatalf("NewTenantDeleter: %v", err)
	}
	err = d.Delete(context.Background(), "tenant-a", "admin-1")
	if err == nil || !strings.Contains(err.Error(), "qdrant down") {
		t.Fatalf("err=%v want sweeper error", err)
	}
	got, gerr := repo.GetStatus(context.Background(), "tenant-a")
	if gerr != nil {
		t.Fatalf("GetStatus: %v", gerr)
	}
	if got != admin.TenantStatusPendingDeletion {
		t.Fatalf("status=%q want pending_deletion (so retry can drive forward)", got)
	}
}

func TestTenantDeleter_RejectsEmptyTenantID(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	d, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{Status: repo})
	if err != nil {
		t.Fatalf("NewTenantDeleter: %v", err)
	}
	err = d.Delete(context.Background(), "", "admin")
	if err == nil || !strings.Contains(err.Error(), "empty tenant_id") {
		t.Fatalf("err=%v want empty tenant_id error", err)
	}
}

func TestTenantDeleter_DrainHonoursContextCancel(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	d, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{
		Status: repo,
		Drain:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewTenantDeleter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = d.Delete(ctx, "tenant-a", "admin")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

func TestNewTenantDeleter_RequiresStatus(t *testing.T) {
	t.Parallel()
	_, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{})
	if err == nil || !strings.Contains(err.Error(), "nil Status") {
		t.Fatalf("err=%v want nil Status error", err)
	}
}

func TestTenantDeleteHandler_DeleteOK(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	sweeper := &recordingTenantSweeper{}
	d, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{
		Status:   repo,
		Sweepers: []admin.TenantSweeper{sweeper},
	})
	if err != nil {
		t.Fatalf("NewTenantDeleter: %v", err)
	}
	h, err := admin.NewTenantDeleteHandler(d)
	if err != nil {
		t.Fatalf("NewTenantDeleteHandler: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Set(audit.ActorContextKey, "admin-1")
	})
	h.Register(g)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/tenants/tenant-a", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"deleted"`) {
		t.Errorf("body missing deleted state: %s", w.Body.String())
	}
}

func TestTenantDeleteHandler_RejectsCrossTenant(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	d, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{Status: repo})
	if err != nil {
		t.Fatalf("NewTenantDeleter: %v", err)
	}
	h, err := admin.NewTenantDeleteHandler(d)
	if err != nil {
		t.Fatalf("NewTenantDeleteHandler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
	})
	h.Register(g)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/tenants/tenant-b", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTenantDeleteHandler_RejectsMissingContext(t *testing.T) {
	t.Parallel()
	repo, _ := newTenantStatusRepo(t)
	d, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{Status: repo})
	if err != nil {
		t.Fatalf("NewTenantDeleter: %v", err)
	}
	h, err := admin.NewTenantDeleteHandler(d)
	if err != nil {
		t.Fatalf("NewTenantDeleteHandler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(&r.RouterGroup)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/tenants/tenant-a", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
