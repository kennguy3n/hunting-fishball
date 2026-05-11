package admin_test

// notification_gorm_test.go — Round-9 Task 1.
//
// SQLite-backed unit tests for the GORM NotificationStore.

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func newNotifSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	return db
}

func newNotificationStoreGORMForTest(t *testing.T) *admin.NotificationStoreGORM {
	t.Helper()
	db := newNotifSQLiteDB(t)
	s, err := admin.NewNotificationStoreGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.AutoMigrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestNotificationStoreGORM_CreateAndList(t *testing.T) {
	t.Parallel()
	s := newNotificationStoreGORMForTest(t)
	pref := &admin.NotificationPreference{
		TenantID: "t-1", EventType: "source.connected", Channel: admin.NotificationChannelWebhook,
		Target: "https://example.com/hook", Enabled: true,
	}
	if err := s.Create(pref); err != nil {
		t.Fatalf("create: %v", err)
	}
	if pref.ID == "" {
		t.Fatalf("expected generated id")
	}
	rows, err := s.List("t-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1; got %d", len(rows))
	}
	if rows[0].Target != "https://example.com/hook" {
		t.Fatalf("unexpected target: %s", rows[0].Target)
	}
}

func TestNotificationStoreGORM_ListByEventOnlyEnabled(t *testing.T) {
	t.Parallel()
	s := newNotificationStoreGORMForTest(t)
	enabled := &admin.NotificationPreference{
		TenantID: "t-1", EventType: "source.connected",
		Channel: admin.NotificationChannelWebhook, Target: "https://a/", Enabled: true,
	}
	disabled := &admin.NotificationPreference{
		TenantID: "t-1", EventType: "source.connected",
		Channel: admin.NotificationChannelWebhook, Target: "https://b/", Enabled: true,
	}
	if err := s.Create(enabled); err != nil {
		t.Fatalf("create enabled: %v", err)
	}
	if err := s.Create(disabled); err != nil {
		t.Fatalf("create disabled: %v", err)
	}
	// Flip the second row to disabled.
	if err := s.Delete("t-1", disabled.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err := s.ListByEvent("t-1", "source.connected")
	if err != nil {
		t.Fatalf("list-by-event: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 enabled row; got %d", len(rows))
	}
	if rows[0].Target != "https://a/" {
		t.Fatalf("unexpected target: %s", rows[0].Target)
	}
}

func TestNotificationStoreGORM_TenantIsolation(t *testing.T) {
	t.Parallel()
	s := newNotificationStoreGORMForTest(t)
	for _, tID := range []string{"t-a", "t-b"} {
		_ = s.Create(&admin.NotificationPreference{
			TenantID: tID, EventType: "audit.event",
			Channel: admin.NotificationChannelWebhook, Target: "https://x/", Enabled: true,
		})
	}
	rows, _ := s.List("t-a")
	if len(rows) != 1 || rows[0].TenantID != "t-a" {
		t.Fatalf("cross-tenant leak: %+v", rows)
	}
}

func TestNotificationStoreGORM_DeleteScopedToTenant(t *testing.T) {
	t.Parallel()
	s := newNotificationStoreGORMForTest(t)
	p := &admin.NotificationPreference{
		TenantID: "t-1", EventType: "audit.event",
		Channel: admin.NotificationChannelEmail, Target: "ops@example.com", Enabled: true,
	}
	if err := s.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete("t-other", p.ID); err == nil {
		t.Fatalf("expected delete from wrong tenant to fail")
	}
	if err := s.Delete("t-1", p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ := s.List("t-1")
	if len(rows) != 0 {
		t.Fatalf("expected empty after delete; got %d", len(rows))
	}
}

func TestNotificationStoreGORM_ValidateOnCreate(t *testing.T) {
	t.Parallel()
	s := newNotificationStoreGORMForTest(t)
	bad := &admin.NotificationPreference{TenantID: "", EventType: "x", Channel: admin.NotificationChannelWebhook, Target: "y"}
	if err := s.Create(bad); err == nil {
		t.Fatalf("expected validation error")
	}
}
