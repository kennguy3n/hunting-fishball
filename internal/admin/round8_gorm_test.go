package admin_test

// round8_gorm_test.go — Round-8 Tasks 6–11.
//
// SQLite-backed unit tests for the GORM stores that replace their
// in-memory predecessors.

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func newSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	return db
}

// ---------- Task 6: QueryAnalyticsStoreGORM ----------

// queryAnalyticsSQLiteDDL mirrors migrations/024_query_analytics.sql
// with SQLite-compatible types (TEXT in place of JSONB, no TIMESTAMPTZ,
// no composite PK on a TIMESTAMP column).
const queryAnalyticsSQLiteDDL = `CREATE TABLE IF NOT EXISTS query_analytics (
	id              TEXT NOT NULL PRIMARY KEY,
	tenant_id       TEXT NOT NULL,
	query_hash      TEXT NOT NULL,
	query_text      TEXT NOT NULL DEFAULT '',
	top_k           INTEGER NOT NULL DEFAULT 0,
	hit_count       INTEGER NOT NULL DEFAULT 0,
	cache_hit       BOOLEAN NOT NULL DEFAULT 0,
	latency_ms      INTEGER NOT NULL DEFAULT 0,
	backend_timings TEXT NOT NULL DEFAULT '{}',
	experiment_name TEXT,
	experiment_arm  TEXT,
	created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func TestQueryAnalyticsStoreGORM_RecordAndList(t *testing.T) {
	t.Parallel()
	db := newSQLiteDB(t)
	if err := db.Exec(queryAnalyticsSQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := admin.NewQueryAnalyticsStoreGORM(db)
	row := &admin.QueryAnalyticsRow{
		TenantID:  "t-1",
		QueryText: "hello world",
		TopK:      10,
		HitCount:  5,
		LatencyMS: 42,
		CacheHit:  false,
	}
	if err := s.Record(context.Background(), row); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := s.List(context.Background(), admin.QueryAnalyticsQuery{TenantID: "t-1", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row; got %d", len(rows))
	}
	if rows[0].QueryText != "hello world" {
		t.Fatalf("wrong query text: %s", rows[0].QueryText)
	}
}

func TestQueryAnalyticsStoreGORM_TenantIsolation(t *testing.T) {
	t.Parallel()
	db := newSQLiteDB(t)
	_ = db.Exec(queryAnalyticsSQLiteDDL).Error
	s := admin.NewQueryAnalyticsStoreGORM(db)
	_ = s.Record(context.Background(), &admin.QueryAnalyticsRow{TenantID: "ta", QueryText: "q", TopK: 5, HitCount: 1, LatencyMS: 10})
	rows, _ := s.List(context.Background(), admin.QueryAnalyticsQuery{TenantID: "tb", Limit: 10})
	if len(rows) != 0 {
		t.Fatalf("cross-tenant leak: %d rows", len(rows))
	}
}

// ---------- Task 7: PinnedResultStoreGORM ----------

const pinnedSQLiteDDL = `CREATE TABLE IF NOT EXISTS pinned_retrieval_results (
	id             TEXT NOT NULL PRIMARY KEY,
	tenant_id      TEXT NOT NULL,
	query_pattern  TEXT NOT NULL,
	chunk_id       TEXT NOT NULL,
	position       INTEGER NOT NULL DEFAULT 0,
	created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func TestPinnedResultStoreGORM_CRUD(t *testing.T) {
	t.Parallel()
	db := newSQLiteDB(t)
	if err := db.Exec(pinnedSQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := admin.NewPinnedResultStoreGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p, err := s.Create(context.Background(), admin.PinnedResult{TenantID: "ta", QueryPattern: "hello", ChunkID: "c1", Position: 0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" {
		t.Fatalf("expected generated id")
	}
	rows, err := s.List(context.Background(), "ta")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1; got %d", len(rows))
	}
	lookup, err := s.Lookup(context.Background(), "ta", "hello")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(lookup) != 1 || lookup[0].ChunkID != "c1" {
		t.Fatalf("lookup mismatch: %+v", lookup)
	}
	if err := s.Delete(context.Background(), "ta", p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ = s.List(context.Background(), "ta")
	if len(rows) != 0 {
		t.Fatalf("expected 0 after delete; got %d", len(rows))
	}
}

// ---------- Task 8: SyncHistoryGORM ----------

func TestSyncHistoryGORM_StartAndFinish(t *testing.T) {
	t.Parallel()
	db := newSQLiteDB(t)
	_ = db.AutoMigrate(&admin.SyncHistoryRow{})
	s, err := admin.NewSyncHistoryGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Start(context.Background(), "t-1", "s-1", "run-1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Finish(context.Background(), "t-1", "s-1", "run-1", admin.SyncStatusSucceeded, 100, 3); err != nil {
		t.Fatalf("finish: %v", err)
	}
	rows, err := s.List(context.Background(), "t-1", "s-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1; got %d", len(rows))
	}
	if rows[0].Status != admin.SyncStatusSucceeded {
		t.Fatalf("expected succeeded; got %s", rows[0].Status)
	}
	if rows[0].DocsProcessed != 100 || rows[0].DocsFailed != 3 {
		t.Fatalf("counts: processed=%d failed=%d", rows[0].DocsProcessed, rows[0].DocsFailed)
	}
}

// ---------- Task 9: LatencyBudgetGORM ----------

func TestLatencyBudgetGORM_PutAndGet(t *testing.T) {
	t.Parallel()
	db := newSQLiteDB(t)
	_ = db.AutoMigrate(&admin.LatencyBudget{})
	s, err := admin.NewLatencyBudgetGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	b := &admin.LatencyBudget{TenantID: "t-1", MaxLatencyMS: 500}
	if err := s.Put(context.Background(), b); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MaxLatencyMS != 500 {
		t.Fatalf("max_latency_ms: %d", got.MaxLatencyMS)
	}
}

// ---------- Task 10: CacheTTLGORM ----------

func TestCacheTTLGORM_PutAndTTLFor(t *testing.T) {
	t.Parallel()
	db := newSQLiteDB(t)
	_ = db.AutoMigrate(&admin.CacheConfig{})
	s, err := admin.NewCacheTTLGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Put(context.Background(), &admin.CacheConfig{TenantID: "t-1", TTLMS: 120000}); err != nil {
		t.Fatalf("put: %v", err)
	}
	ttl := s.TTLFor(context.Background(), "t-1", 5*time.Minute)
	if ttl != 120*time.Second {
		t.Fatalf("expected 120s; got %v", ttl)
	}
	// Fallback for unknown tenant:
	fallback := s.TTLFor(context.Background(), "unknown", 5*time.Minute)
	if fallback != 5*time.Minute {
		t.Fatalf("expected fallback 5m; got %v", fallback)
	}
}

// ---------- Task 11: CredentialHealthGORM ----------

func TestCredentialHealthGORM_RecordAndGet(t *testing.T) {
	t.Parallel()
	db := newSQLiteDB(t)

	// The CredentialHealthGORM writes to source_health which is
	// initially created by migration 003. Simulate it for SQLite.
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS source_health (
		tenant_id   TEXT NOT NULL,
		source_id   TEXT NOT NULL,
		last_success_at DATETIME,
		last_failure_at DATETIME,
		lag         INTEGER NOT NULL DEFAULT 0,
		error_count INTEGER NOT NULL DEFAULT 0,
		status      TEXT NOT NULL DEFAULT 'unknown',
		updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		credential_valid     BOOLEAN NOT NULL DEFAULT 1,
		credential_checked_at DATETIME,
		credential_error     TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (tenant_id, source_id)
	)`).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}

	s, err := admin.NewCredentialHealthGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.RecordCredentialCheck(context.Background(), "t-1", "s-1", true, ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	row, found := s.Get(context.Background(), "t-1", "s-1")
	if !found {
		t.Fatalf("expected found")
	}
	if !row.Valid {
		t.Fatalf("expected valid=true")
	}
	// Update to invalid via raw SQL (gorm.OnConflict doesn't always
	// rewrite to SQLite's INSERT … ON CONFLICT DO UPDATE the way the
	// Postgres clause expects). The production database is Postgres;
	// here we just verify Get reflects the latest row.
	if err := db.Exec(`UPDATE source_health SET credential_valid=0, credential_error=?, credential_checked_at=? WHERE tenant_id=? AND source_id=?`,
		"token expired", time.Now().UTC(), "t-1", "s-1").Error; err != nil {
		t.Fatalf("update invalid: %v", err)
	}
	row, _ = s.Get(context.Background(), "t-1", "s-1")
	if row.Valid {
		t.Fatalf("expected valid=false")
	}
	if row.Message != "token expired" {
		t.Fatalf("wrong message: %s", row.Message)
	}
}
