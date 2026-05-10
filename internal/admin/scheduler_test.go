package admin_test

import (
	"context"
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

func newSchedulerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&admin.SyncSchedule{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

type recordingEmitter struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingEmitter) EmitSync(_ context.Context, tenantID, sourceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, tenantID+":"+sourceID)
	return nil
}

func TestScheduler_TickFiresDueSchedules(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)
	now := time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC)

	if _, err := admin.UpsertSchedule(context.Background(), db, "tenant-a", "src-a", "@every 5m", true, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emit := &recordingEmitter{}
	s, err := admin.NewScheduler(admin.SchedulerConfig{
		DB:      db,
		Emitter: emit,
		Now:     func() time.Time { return now.Add(10 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := len(emit.events); got != 1 {
		t.Fatalf("expected 1 emit, got %d", got)
	}
	if emit.events[0] != "tenant-a:src-a" {
		t.Fatalf("emit event = %q", emit.events[0])
	}
	row, err := admin.GetSchedule(context.Background(), db, "tenant-a", "src-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !row.NextRunAt.After(now.Add(10 * time.Minute)) {
		t.Fatalf("next_run_at not advanced: %s", row.NextRunAt)
	}
	if row.LastRunAt.IsZero() {
		t.Fatalf("last_run_at not set")
	}
}

func TestScheduler_DisabledScheduleSkipped(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)
	now := time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC)

	if _, err := admin.UpsertSchedule(context.Background(), db, "tenant-a", "src-a", "@every 1m", false, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emit := &recordingEmitter{}
	s, _ := admin.NewScheduler(admin.SchedulerConfig{
		DB: db, Emitter: emit, Now: func() time.Time { return now.Add(10 * time.Minute) },
	})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := len(emit.events); got != 0 {
		t.Fatalf("expected 0 emits for disabled schedule, got %d", got)
	}
}

func TestScheduler_BadCronRejected(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)
	if _, err := admin.UpsertSchedule(context.Background(), db, "tenant-a", "src-a", "not a cron", true, time.Now()); err == nil {
		t.Fatalf("expected parse error for bad cron expression")
	}
}

func TestSchedulerHandler_CRUD(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	g := r.Group("")
	admin.NewSchedulerHandler(db).Register(g)

	// Upsert
	body := `{"cron_expr":"@every 5m"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/src-1/schedule", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upsert status = %d, body = %s", w.Code, w.Body.String())
	}

	// Get
	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/schedule", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "@every 5m") {
		t.Fatalf("get body missing cron: %s", w2.Body.String())
	}

	// Delete
	req3 := httptest.NewRequest(http.MethodDelete, "/v1/admin/sources/src-1/schedule", nil)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", w3.Code)
	}

	// Get after delete -> 404
	req4 := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/schedule", nil)
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d", w4.Code)
	}
}

func TestSchedulerHandler_BadRequest(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	g := r.Group("")
	admin.NewSchedulerHandler(db).Register(g)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/src-1/schedule", strings.NewReader(`{"cron_expr":"bogus"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestScheduler_UpsertValidationErrorWrapsSentinel locks down the
// contract: validation failures (missing tenant/source, bad cron
// expression, cron yields no future fire) wrap ErrScheduleValidation
// so the HTTP layer can map them to 400. Without this guarantee, a
// caller-side fault would land in the 5xx alert bucket.
func TestScheduler_UpsertValidationErrorWrapsSentinel(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)
	cases := []struct {
		name     string
		tenantID string
		sourceID string
		expr     string
	}{
		{"missing-tenant", "", "src-a", "@every 5m"},
		{"missing-source", "tenant-a", "", "@every 5m"},
		{"bad-cron", "tenant-a", "src-a", "this is not a cron expression"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.UpsertSchedule(context.Background(), db, tc.tenantID, tc.sourceID, tc.expr, true, time.Now())
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, admin.ErrScheduleValidation) {
				t.Fatalf("error %q does not wrap ErrScheduleValidation", err)
			}
		})
	}
}

// TestSchedulerHandler_DBErrorIs500 closes the underlying DB
// connection so any GORM call fails, then asserts the upsert
// handler returns 500 (server-side fault) — not 400 (caller-side).
// This locks in the fix for the Devin Review finding that mapped
// every UpsertSchedule error to 400, hiding infra failures from
// 5xx alerting.
func TestSchedulerHandler_DBErrorIs500(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	g := r.Group("")
	admin.NewSchedulerHandler(db).Register(g)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/src-1/schedule",
		strings.NewReader(`{"cron_expr":"@every 5m"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on DB failure, got %d (body=%s)", w.Code, w.Body.String())
	}
}

// TestSchedulerHandler_ValidationErrorIs400 confirms a bad cron
// expression still maps to 400 after the sentinel-wrapping change.
// Complements TestSchedulerHandler_BadRequest by inspecting the
// sentinel directly via UpsertSchedule and exercising the HTTP
// switch arm.
func TestSchedulerHandler_ValidationErrorIs400(t *testing.T) {
	t.Parallel()
	db := newSchedulerDB(t)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	g := r.Group("")
	admin.NewSchedulerHandler(db).Register(g)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/src-1/schedule",
		strings.NewReader(`{"cron_expr":"not-a-cron-expr"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad cron, got %d (body=%s)", w.Code, w.Body.String())
	}
}
