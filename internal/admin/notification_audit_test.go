package admin_test

// notification_audit_test.go — Round-8 Task 5.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type round8RecordingAudit struct {
	mu   sync.Mutex
	rows []*audit.AuditLog
}

func (r *round8RecordingAudit) Create(_ context.Context, log *audit.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, log)
	return nil
}

type capturingDelivery struct {
	mu      sync.Mutex
	targets []string
	attempt int32
}

func (c *capturingDelivery) Send(_ context.Context, target string, _ admin.NotificationChannel, _ []byte) (admin.DeliveryResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets = append(c.targets, target)
	a := atomic.AddInt32(&c.attempt, 1)
	return admin.DeliveryResult{Attempts: int(a), StatusCode: 200}, nil
}

func TestNotifyingAuditWriter_FiresOnMatchingSubscription(t *testing.T) {
	t.Parallel()

	rec := &round8RecordingAudit{}
	store := admin.NewInMemoryNotificationStore()
	if err := store.Create(&admin.NotificationPreference{
		TenantID: "t-1", EventType: string(audit.ActionSourceConnected),
		Channel: admin.NotificationChannelWebhook, Target: "https://hook.example/match", Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Non-matching subscription must not fire.
	_ = store.Create(&admin.NotificationPreference{
		TenantID: "t-1", EventType: "other.event",
		Channel: admin.NotificationChannelWebhook, Target: "https://hook.example/other", Enabled: true,
	})
	// Cross-tenant subscription must not fire.
	_ = store.Create(&admin.NotificationPreference{
		TenantID: "t-2", EventType: string(audit.ActionSourceConnected),
		Channel: admin.NotificationChannelWebhook, Target: "https://hook.example/cross", Enabled: true,
	})

	delivery := &capturingDelivery{}
	dispatcher, err := admin.NewNotificationDispatcher(store, delivery)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	naw := &admin.NotifyingAuditWriter{Inner: rec, Dispatcher: dispatcher, Async: false}

	log := &audit.AuditLog{
		ID:           "01J123456789012345678901AB",
		TenantID:     "t-1",
		ActorID:      "01J123456789012345678901XX",
		Action:       audit.ActionSourceConnected,
		ResourceType: "source",
		ResourceID:   "01J123456789012345678901CD",
		Metadata:     audit.JSONMap{"k": "v"},
	}
	if err := naw.Create(context.Background(), log); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(rec.rows); got != 1 {
		t.Fatalf("audit row not persisted: %d", got)
	}
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if len(delivery.targets) != 1 {
		t.Fatalf("expected exactly one delivery; got %v", delivery.targets)
	}
	if delivery.targets[0] != "https://hook.example/match" {
		t.Fatalf("wrong target: %s", delivery.targets[0])
	}
}

func TestNotifyingAuditWriter_NoSubscription_NoDelivery(t *testing.T) {
	t.Parallel()
	rec := &round8RecordingAudit{}
	store := admin.NewInMemoryNotificationStore()
	delivery := &capturingDelivery{}
	dispatcher, _ := admin.NewNotificationDispatcher(store, delivery)
	naw := &admin.NotifyingAuditWriter{Inner: rec, Dispatcher: dispatcher, Async: false}

	if err := naw.Create(context.Background(), &audit.AuditLog{
		ID: "01J123456789012345678901AB", TenantID: "t", Action: audit.ActionSourceConnected,
		ResourceType: "source", ResourceID: "01J123456789012345678901CD",
		Metadata: audit.JSONMap{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if len(delivery.targets) != 0 {
		t.Fatalf("unexpected deliveries: %v", delivery.targets)
	}
}

func TestNotifyingAuditWriter_AsyncDispatch(t *testing.T) {
	t.Parallel()
	rec := &round8RecordingAudit{}
	store := admin.NewInMemoryNotificationStore()
	_ = store.Create(&admin.NotificationPreference{
		TenantID: "t", EventType: string(audit.ActionPolicyPromoted),
		Channel: admin.NotificationChannelWebhook, Target: "https://x", Enabled: true,
	})
	delivery := &capturingDelivery{}
	dispatcher, _ := admin.NewNotificationDispatcher(store, delivery)
	naw := &admin.NotifyingAuditWriter{Inner: rec, Dispatcher: dispatcher, Async: true}
	if err := naw.Create(context.Background(), &audit.AuditLog{
		ID: "01J123456789012345678901AB", TenantID: "t", Action: audit.ActionPolicyPromoted,
		ResourceType: "policy", ResourceID: "01J123456789012345678901CD",
		Metadata: audit.JSONMap{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Async dispatch — wait briefly for the goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		delivery.mu.Lock()
		n := len(delivery.targets)
		delivery.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("async dispatch never fired")
}
