package admin_test

// abtest_gorm_test.go — Round-9 Task 2.

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// abtestSQLiteDDL mirrors migrations/021_ab_tests.sql with
// SQLite-compatible types (TEXT in place of JSONB, no TIMESTAMPTZ).
// JSONMap.Scan/Value still operate over TEXT JSON strings, so the
// round-trip is faithful to the production behaviour.
const abtestSQLiteDDL = `CREATE TABLE IF NOT EXISTS retrieval_ab_tests (
tenant_id              TEXT NOT NULL,
experiment_name        TEXT NOT NULL,
status                 TEXT NOT NULL DEFAULT 'draft',
traffic_split_percent  INTEGER NOT NULL DEFAULT 0,
control_config         TEXT NOT NULL DEFAULT '{}',
variant_config         TEXT NOT NULL DEFAULT '{}',
start_at               DATETIME,
end_at                 DATETIME,
created_at             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
updated_at             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
PRIMARY KEY (tenant_id, experiment_name)
);`

func newABTestStoreGORMForTest(t *testing.T) *admin.ABTestStoreGORM {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.Exec(abtestSQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := admin.NewABTestStoreGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

func TestABTestStoreGORM_UpsertAndGet(t *testing.T) {
	t.Parallel()
	s := newABTestStoreGORMForTest(t)
	cfg := &admin.ABTestConfig{
		TenantID: "t-1", ExperimentName: "rerank-v2",
		Status: admin.ABTestStatusActive, TrafficSplitPercent: 50,
		ControlConfig: map[string]any{"reranker": "off"},
		VariantConfig: map[string]any{"reranker": "on"},
	}
	if err := s.Upsert(cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.Get("t-1", "rerank-v2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != admin.ABTestStatusActive {
		t.Fatalf("status: %s", got.Status)
	}
	if got.TrafficSplitPercent != 50 {
		t.Fatalf("split: %d", got.TrafficSplitPercent)
	}
	if got.ControlConfig["reranker"] != "off" {
		t.Fatalf("control: %+v", got.ControlConfig)
	}
}

func TestABTestStoreGORM_UpsertReplaces(t *testing.T) {
	t.Parallel()
	s := newABTestStoreGORMForTest(t)
	cfg := &admin.ABTestConfig{
		TenantID: "t-1", ExperimentName: "rerank-v2",
		Status: admin.ABTestStatusDraft, TrafficSplitPercent: 10,
	}
	_ = s.Upsert(cfg)
	cfg.Status = admin.ABTestStatusEnded
	cfg.TrafficSplitPercent = 0
	if err := s.Upsert(cfg); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _ := s.Get("t-1", "rerank-v2")
	if got.Status != admin.ABTestStatusEnded {
		t.Fatalf("status after upsert: %s", got.Status)
	}
}

func TestABTestStoreGORM_List(t *testing.T) {
	t.Parallel()
	s := newABTestStoreGORMForTest(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		_ = s.Upsert(&admin.ABTestConfig{
			TenantID: "t-1", ExperimentName: name,
			Status: admin.ABTestStatusDraft, TrafficSplitPercent: 0,
		})
	}
	rows, err := s.List("t-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(rows))
	}
}

func TestABTestStoreGORM_TenantIsolation(t *testing.T) {
	t.Parallel()
	s := newABTestStoreGORMForTest(t)
	_ = s.Upsert(&admin.ABTestConfig{TenantID: "ta", ExperimentName: "x", Status: admin.ABTestStatusDraft})
	_ = s.Upsert(&admin.ABTestConfig{TenantID: "tb", ExperimentName: "x", Status: admin.ABTestStatusDraft})
	rowsA, _ := s.List("ta")
	if len(rowsA) != 1 || rowsA[0].TenantID != "ta" {
		t.Fatalf("cross-tenant leak: %+v", rowsA)
	}
}

func TestABTestStoreGORM_Delete(t *testing.T) {
	t.Parallel()
	s := newABTestStoreGORMForTest(t)
	_ = s.Upsert(&admin.ABTestConfig{TenantID: "t-1", ExperimentName: "x", Status: admin.ABTestStatusDraft})
	if err := s.Delete("t-1", "x"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get("t-1", "x"); err == nil {
		t.Fatalf("expected not-found after delete")
	}
}

func TestABTestStoreGORM_GetMissing(t *testing.T) {
	t.Parallel()
	s := newABTestStoreGORMForTest(t)
	if _, err := s.Get("t-1", "missing"); err == nil {
		t.Fatalf("expected not-found error")
	}
}
