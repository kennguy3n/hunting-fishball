package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// sqliteHealthSchema mirrors migrations/003_source_health.sql in
// SQLite-compatible types.
const sqliteHealthSchema = `
CREATE TABLE source_health (
    tenant_id        TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    last_success_at  DATETIME,
    last_failure_at  DATETIME,
    lag              INTEGER NOT NULL DEFAULT 0,
    error_count      INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'unknown',
    updated_at       DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, source_id)
);
CREATE INDEX idx_source_health_tenant_status ON source_health (tenant_id, status);
`

func newSQLiteHealthRepo(t *testing.T) (*admin.HealthRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteHealthSchema).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return admin.NewHealthRepository(db, admin.HealthThresholds{
		LagWarn: 10, LagAlert: 100,
		ErrorWarn: 2, ErrorAlert: 5,
		StaleSuccess: time.Hour,
	}), db
}

func TestHealth_Unknown_OnNewSource(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteHealthRepo(t)
	ctx := context.Background()
	got, err := repo.Get(ctx, "tenant-a", "src-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil row, got %+v", got)
	}
}

func TestHealth_RecordSuccess_Healthy(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteHealthRepo(t)
	ctx := context.Background()
	if err := repo.RecordSuccess(ctx, "tenant-a", "src-1"); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}
	got, _ := repo.Get(ctx, "tenant-a", "src-1")
	if got == nil || got.Status != admin.HealthStatusHealthy {
		t.Fatalf("Status: %+v", got)
	}
	if got.LastSuccessAt == nil {
		t.Fatal("LastSuccessAt must be set")
	}
}

func TestHealth_RecordFailure_DegradedThenFailing(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteHealthRepo(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_ = repo.RecordFailure(ctx, "tenant-a", "src-1")
	}
	got, _ := repo.Get(ctx, "tenant-a", "src-1")
	if got.Status != admin.HealthStatusDegraded {
		t.Fatalf("expected degraded after 2 errors, got %q", got.Status)
	}
	for i := 0; i < 5; i++ {
		_ = repo.RecordFailure(ctx, "tenant-a", "src-1")
	}
	got, _ = repo.Get(ctx, "tenant-a", "src-1")
	if got.Status != admin.HealthStatusFailing {
		t.Fatalf("expected failing after 7 errors, got %q", got.Status)
	}
}

func TestHealth_SuccessResetsErrorCount(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteHealthRepo(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = repo.RecordFailure(ctx, "tenant-a", "src-1")
	}
	if err := repo.RecordSuccess(ctx, "tenant-a", "src-1"); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}
	got, _ := repo.Get(ctx, "tenant-a", "src-1")
	if got.ErrorCount != 0 || got.Status != admin.HealthStatusHealthy {
		t.Fatalf("success must reset rolling errors: %+v", got)
	}
}

func TestHealth_LagThresholds(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteHealthRepo(t)
	ctx := context.Background()
	_ = repo.RecordSuccess(ctx, "tenant-a", "src-1")
	if err := repo.SetLag(ctx, "tenant-a", "src-1", 50); err != nil {
		t.Fatalf("SetLag: %v", err)
	}
	got, _ := repo.Get(ctx, "tenant-a", "src-1")
	if got.Status != admin.HealthStatusDegraded {
		t.Fatalf("lag=50 should be degraded, got %q", got.Status)
	}
	if err := repo.SetLag(ctx, "tenant-a", "src-1", 500); err != nil {
		t.Fatalf("SetLag: %v", err)
	}
	got, _ = repo.Get(ctx, "tenant-a", "src-1")
	if got.Status != admin.HealthStatusFailing {
		t.Fatalf("lag=500 should be failing, got %q", got.Status)
	}
}

func TestHealth_TenantIsolation(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteHealthRepo(t)
	ctx := context.Background()
	_ = repo.RecordSuccess(ctx, "tenant-a", "src-1")
	got, _ := repo.Get(ctx, "tenant-b", "src-1")
	if got != nil {
		t.Fatalf("cross-tenant Get returned a row: %+v", got)
	}
}

func TestDeriveStatus_StaleSuccess(t *testing.T) {
	t.Parallel()
	long := time.Now().UTC().Add(-2 * time.Hour)
	h := &admin.Health{LastSuccessAt: &long}
	got := admin.DeriveStatus(h, admin.HealthThresholds{StaleSuccess: time.Hour}, time.Now().UTC())
	if got != admin.HealthStatusFailing {
		t.Fatalf("stale-success must mark failing, got %q", got)
	}
}
