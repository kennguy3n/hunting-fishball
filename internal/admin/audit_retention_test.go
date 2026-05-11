// Round-12 Task 17 — tests for the audit retention sweeper.
//
// We open an in-memory SQLite database via the GORM dialector
// (same path the production wiring uses through gorm.Open), seed
// a mix of stale and fresh rows, and verify Tick deletes only the
// stale rows and increments the metric. Using GORM here matches
// the production driver (pgx) at the placeholder-translation
// layer, so the same SQL the sweeper issues at runtime is the SQL
// the test executes.
package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

func newAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Exec(`
		CREATE TABLE audit_logs (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			action TEXT NOT NULL,
			created_at DATETIME NOT NULL
		)
	`).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func insertAudit(t *testing.T, db *gorm.DB, id string, createdAt time.Time) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO audit_logs (id, tenant_id, action, created_at) VALUES (?, ?, ?, ?)`,
		id, "tenant-a", "test", createdAt,
	).Error; err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func countAudit(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM audit_logs`).Scan(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return int(n)
}

// TestAuditRetention_DeletesOldRowsAndKeepsFresh seeds 3 stale + 2
// fresh rows, runs Tick once, and verifies only the stale rows
// were removed and the metric was incremented by 3.
func TestAuditRetention_DeletesOldRowsAndKeepsFresh(t *testing.T) {
	// No t.Parallel(): observability.AuditRowsExpiredTotal is
	// a package-global counter shared with the other two tests
	// in this file. Running these in parallel races on the
	// delta the test measures.
	db := newAuditDB(t)
	observability.AuditRowsExpiredTotal.Add(0) // touch to ensure registered
	before := testutil.ToFloat64(observability.AuditRowsExpiredTotal)

	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * 24 * time.Hour) // older than 90d default
	fresh := now.Add(-10 * 24 * time.Hour)  // within retention

	insertAudit(t, db, "s1", stale)
	insertAudit(t, db, "s2", stale)
	insertAudit(t, db, "s3", stale)
	insertAudit(t, db, "f1", fresh)
	insertAudit(t, db, "f2", fresh)

	sw, err := admin.NewAuditRetentionSweeper(admin.AuditRetentionConfig{
		DB:        db,
		BatchSize: 2, // exercise the batching loop
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	if err := sw.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := countAudit(t, db); got != 2 {
		t.Fatalf("remaining rows = %d, want 2 (fresh only)", got)
	}
	after := testutil.ToFloat64(observability.AuditRowsExpiredTotal)
	if diff := after - before; diff != 3 {
		t.Fatalf("metric delta = %v, want 3", diff)
	}
}

// TestAuditRetention_RespectsCustomRetentionWindow uses a 1-day
// window so a row that is 2 days old becomes stale even though
// the default would consider it fresh.
func TestAuditRetention_RespectsCustomRetentionWindow(t *testing.T) {
	db := newAuditDB(t)
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	twoDays := now.Add(-2 * 24 * time.Hour)
	insertAudit(t, db, "x1", twoDays)
	insertAudit(t, db, "x2", now.Add(-30*time.Minute))

	sw, err := admin.NewAuditRetentionSweeper(admin.AuditRetentionConfig{
		DB:              db,
		RetentionWindow: 24 * time.Hour,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	if err := sw.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := countAudit(t, db); got != 1 {
		t.Fatalf("remaining rows = %d, want 1", got)
	}
}

// TestAuditRetention_NoRowsToDeleteIsNoOp ensures a clean sweep
// neither errors nor moves the metric.
func TestAuditRetention_NoRowsToDeleteIsNoOp(t *testing.T) {
	db := newAuditDB(t)
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	insertAudit(t, db, "y1", now.Add(-1*time.Hour))

	before := testutil.ToFloat64(observability.AuditRowsExpiredTotal)
	sw, err := admin.NewAuditRetentionSweeper(admin.AuditRetentionConfig{
		DB:  db,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	if err := sw.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := countAudit(t, db); got != 1 {
		t.Fatalf("remaining rows = %d, want 1 (none expired)", got)
	}
	after := testutil.ToFloat64(observability.AuditRowsExpiredTotal)
	if diff := after - before; diff != 0 {
		t.Fatalf("metric delta = %v, want 0", diff)
	}
}
