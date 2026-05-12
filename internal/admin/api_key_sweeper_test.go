package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type sweepAuditRec struct {
	logs []*audit.AuditLog
}

func (s *sweepAuditRec) Create(_ context.Context, l *audit.AuditLog) error {
	s.logs = append(s.logs, l)
	return nil
}

func newSweeperDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(&admin.APIKeyRow{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestAPIKeySweeper_ExpiresOnlyDueRows(t *testing.T) {
	db := newSweeperDB(t)
	store := admin.NewAPIKeyStoreGORM(db)

	now := time.Now().UTC()
	rows := []*admin.APIKeyRow{
		// Active — should stay active.
		{ID: ulid.Make().String(), TenantID: "t-a", KeyHash: "h1", Status: string(admin.APIKeyStatusActive), CreatedAt: now},
		// Grace but not yet due — should stay grace.
		{ID: ulid.Make().String(), TenantID: "t-a", KeyHash: "h2", Status: string(admin.APIKeyStatusGrace), CreatedAt: now, GraceUntil: timePtr(now.Add(time.Hour))},
		// Grace and overdue — should flip to expired.
		{ID: ulid.Make().String(), TenantID: "t-b", KeyHash: "h3", Status: string(admin.APIKeyStatusGrace), CreatedAt: now, GraceUntil: timePtr(now.Add(-time.Hour))},
	}
	for _, r := range rows {
		if err := store.Insert(context.Background(), r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rec := &sweepAuditRec{}
	sw, err := admin.NewAPIKeySweeper(admin.APIKeySweeperConfig{
		Store: store, Audit: rec, Interval: time.Minute,
		NowFn: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	n, err := sw.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("transitioned=%d want 1", n)
	}
	if len(rec.logs) != 1 || rec.logs[0].Action != audit.ActionAPIKeyExpired {
		t.Fatalf("audit emit wrong: %+v", rec.logs)
	}

	// Verify state in DB.
	var stored []admin.APIKeyRow
	if err := db.Find(&stored).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	byHash := map[string]string{}
	for _, r := range stored {
		byHash[r.KeyHash] = r.Status
	}
	if byHash["h1"] != string(admin.APIKeyStatusActive) {
		t.Fatalf("h1 status=%q", byHash["h1"])
	}
	if byHash["h2"] != string(admin.APIKeyStatusGrace) {
		t.Fatalf("h2 status=%q", byHash["h2"])
	}
	if byHash["h3"] != string(admin.APIKeyStatusExpired) {
		t.Fatalf("h3 status=%q", byHash["h3"])
	}
}

func TestAPIKeySweeper_IdempotentOnEmptyQueue(t *testing.T) {
	db := newSweeperDB(t)
	store := admin.NewAPIKeyStoreGORM(db)
	sw, err := admin.NewAPIKeySweeper(admin.APIKeySweeperConfig{Store: store, NowFn: func() time.Time { return time.Now() }})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	n, err := sw.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("got %d want 0", n)
	}
}

func TestAPIKeySweepInterval_DefaultAndOverride(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_API_KEY_SWEEP_INTERVAL", "")
	if got := admin.APIKeySweepInterval(); got != 5*time.Minute {
		t.Fatalf("default=%v", got)
	}
	t.Setenv("CONTEXT_ENGINE_API_KEY_SWEEP_INTERVAL", "30s")
	if got := admin.APIKeySweepInterval(); got != 30*time.Second {
		t.Fatalf("override=%v", got)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
