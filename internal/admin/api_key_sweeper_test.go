package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/oklog/ulid/v2"
	dto "github.com/prometheus/client_model/go"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
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

// TestAPIKeySweeper_PopulatesGraceExpiringSoonGauge — Round-14 PR
// review fix. Before this test the
// context_engine_api_keys_grace_expiring_soon gauge was declared
// but never set, so the matching alert in deploy/alerts.yaml
// could not fire. The sweeper now publishes a count of rows whose
// grace_until lies within one hour of now on every SweepOnce call.
func TestAPIKeySweeper_PopulatesGraceExpiringSoonGauge(t *testing.T) {
	observability.ResetForTest()
	db := newSweeperDB(t)
	store := admin.NewAPIKeyStoreGORM(db)

	now := time.Now().UTC()
	rows := []*admin.APIKeyRow{
		// Active — never counted.
		{ID: ulid.Make().String(), TenantID: "t-a", KeyHash: "h1", Status: string(admin.APIKeyStatusActive), CreatedAt: now},
		// Grace, expires in 30m — should be counted (within 1h window).
		{ID: ulid.Make().String(), TenantID: "t-a", KeyHash: "h2", Status: string(admin.APIKeyStatusGrace), CreatedAt: now, GraceUntil: timePtr(now.Add(30 * time.Minute))},
		// Grace, expires in 45m — should also be counted.
		{ID: ulid.Make().String(), TenantID: "t-b", KeyHash: "h3", Status: string(admin.APIKeyStatusGrace), CreatedAt: now, GraceUntil: timePtr(now.Add(45 * time.Minute))},
		// Grace but well beyond the 1h window — excluded.
		{ID: ulid.Make().String(), TenantID: "t-b", KeyHash: "h4", Status: string(admin.APIKeyStatusGrace), CreatedAt: now, GraceUntil: timePtr(now.Add(6 * time.Hour))},
		// Grace and already overdue — gets expired by ExpireGrace, not counted.
		{ID: ulid.Make().String(), TenantID: "t-c", KeyHash: "h5", Status: string(admin.APIKeyStatusGrace), CreatedAt: now, GraceUntil: timePtr(now.Add(-time.Hour))},
	}
	for _, r := range rows {
		if err := store.Insert(context.Background(), r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	sw, err := admin.NewAPIKeySweeper(admin.APIKeySweeperConfig{
		Store: store, Interval: time.Minute,
		NowFn: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	if _, err := sw.SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := plainGaugeValue(t, observability.APIKeysGraceExpiringSoon); got != 2 {
		t.Fatalf("grace_expiring_soon gauge=%v want 2", got)
	}
}

func plainGaugeValue(t *testing.T, g interface {
	Write(*dto.Metric) error
},
) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge write: %v", err)
	}
	return m.GetGauge().GetValue()
}
