package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// sqliteSchema is a hand-rolled CREATE TABLE statement matching the
// audit_logs columns from internal/audit/model.go. The production
// schema lives in migrations/001_audit_log.sql and uses Postgres
// types (jsonb, char(26), tsz). We can't run that DDL against SQLite,
// so we mirror the column shape here in SQLite-compatible types
// (TEXT/JSONB-as-TEXT). Repository behaviour is identical for the
// queries the tests exercise.
const sqliteSchema = `
CREATE TABLE audit_logs (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    actor_id      TEXT,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT,
    metadata      TEXT NOT NULL DEFAULT '{}',
    trace_id      TEXT,
    created_at    DATETIME NOT NULL,
    published_at  DATETIME
);
CREATE INDEX idx_audit_tenant_created ON audit_logs (tenant_id, id DESC);
CREATE INDEX idx_audit_unpublished ON audit_logs (id) WHERE published_at IS NULL;
`

// newSQLiteRepo spins up an in-memory SQLite-backed *gorm.DB with the
// audit_logs schema applied. SQLite is good enough for repository
// behaviour; testcontainers PG is overkill for this layer.
func newSQLiteRepo(t *testing.T) (*audit.Repository, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteSchema).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	return audit.NewRepository(db), db
}

func TestRepository_CreateAndGet(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	log := audit.NewAuditLog(
		"tenant-a", "actor-1",
		audit.ActionConnectorConnected,
		"source", "src-1",
		audit.JSONMap{"connector": "google-drive"},
		"trace-x",
	)
	if err := repo.Create(ctx, log); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, "tenant-a", log.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Action != audit.ActionConnectorConnected {
		t.Fatalf("Action mismatch: %q", got.Action)
	}
	if got.Metadata["connector"] != "google-drive" {
		t.Fatalf("Metadata not round-tripped: %+v", got.Metadata)
	}
}

func TestRepository_TenantIsolationGet(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	log := audit.NewAuditLog("tenant-a", "actor-1", audit.ActionAuditRead, "audit", "x", nil, "")
	if err := repo.Create(ctx, log); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := repo.Get(ctx, "tenant-b", log.ID); err == nil {
		t.Fatal("cross-tenant Get must not return the row")
	}
}

func TestRepository_CreateInTx_RollbackDropsRow(t *testing.T) {
	t.Parallel()

	repo, db := newSQLiteRepo(t)
	ctx := context.Background()

	log := audit.NewAuditLog("tenant-a", "actor-1", audit.ActionSourceSynced, "source", "src-1", nil, "")
	tx := db.Begin()
	if err := repo.CreateInTx(ctx, tx, log); err != nil {
		tx.Rollback()
		t.Fatalf("CreateInTx: %v", err)
	}
	tx.Rollback()

	if _, err := repo.Get(ctx, "tenant-a", log.ID); err == nil {
		t.Fatal("rolled-back row must not be visible")
	}
}

func TestRepository_ListPagination(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		log := audit.NewAuditLog("tenant-a", "actor-1", audit.ActionSourceSynced, "source", "src", nil, "")
		// Force monotonic CreatedAt so the page assertions are stable.
		log.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := repo.Create(ctx, log); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	page1, err := repo.List(ctx, audit.ListFilter{TenantID: "tenant-a", PageSize: 2})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("page1 size: got %d", len(page1.Items))
	}
	if page1.NextPageToken == "" {
		t.Fatal("missing NextPageToken on full page")
	}

	page2, err := repo.List(ctx, audit.ListFilter{TenantID: "tenant-a", PageSize: 2, PageToken: page1.NextPageToken})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(page2.Items) != 2 {
		t.Fatalf("page2 size: got %d", len(page2.Items))
	}

	// Final page: 1 item, no next token.
	page3, err := repo.List(ctx, audit.ListFilter{TenantID: "tenant-a", PageSize: 2, PageToken: page2.NextPageToken})
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(page3.Items) != 1 || page3.NextPageToken != "" {
		t.Fatalf("page3: items=%d token=%q", len(page3.Items), page3.NextPageToken)
	}
}

func TestRepository_ListFilters(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	for i, action := range []audit.Action{
		audit.ActionSourceSynced,
		audit.ActionRetrievalQueried,
		audit.ActionRetrievalQueried,
	} {
		log := audit.NewAuditLog("tenant-a", "actor-1", action, "source", "src", nil, "")
		log.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := repo.Create(ctx, log); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	got, err := repo.List(ctx, audit.ListFilter{
		TenantID: "tenant-a",
		Actions:  []audit.Action{audit.ActionRetrievalQueried},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 retrieval rows, got %d", len(got.Items))
	}
	for _, row := range got.Items {
		if row.Action != audit.ActionRetrievalQueried {
			t.Fatalf("filter leak: %+v", row)
		}
	}
}

func TestRepository_PendingPublishAndMark(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		log := audit.NewAuditLog("tenant-a", "", audit.ActionSourceSynced, "source", "src", nil, "")
		log.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := repo.Create(ctx, log); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	pending, err := repo.PendingPublish(ctx, 10)
	if err != nil {
		t.Fatalf("PendingPublish: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending, got %d", len(pending))
	}

	now := time.Now().UTC()
	ids := []string{pending[0].ID, pending[1].ID}
	if err := repo.MarkPublished(ctx, ids, now); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}

	remaining, err := repo.PendingPublish(ctx, 10)
	if err != nil {
		t.Fatalf("PendingPublish: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 pending after mark, got %d", len(remaining))
	}
	if remaining[0].ID != pending[2].ID {
		t.Fatalf("wrong remaining row: %s", remaining[0].ID)
	}
}

func TestRepository_ListPayloadSearch(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	matched := audit.NewAuditLog(
		"tenant-a", "actor-1",
		audit.ActionConnectorConnected,
		"source", "src-1",
		audit.JSONMap{"connector": "google-drive"},
		"",
	)
	other := audit.NewAuditLog(
		"tenant-a", "actor-1",
		audit.ActionConnectorConnected,
		"source", "src-2",
		audit.JSONMap{"connector": "github"},
		"",
	)
	for i, log := range []*audit.AuditLog{matched, other} {
		log.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := repo.Create(ctx, log); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	got, err := repo.List(ctx, audit.ListFilter{
		TenantID:      "tenant-a",
		PayloadSearch: "google-drive",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("expected 1 row matching payload, got %d", len(got.Items))
	}
	if got.Items[0].ID != matched.ID {
		t.Fatalf("wrong row matched: %s want %s", got.Items[0].ID, matched.ID)
	}
}

func TestRepository_RejectsMissingTenant(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	if _, err := repo.List(ctx, audit.ListFilter{}); err == nil {
		t.Fatal("List must require a tenant id")
	}
}
