package admin_test

// notification_audit_test.go — Round-8 Task 5.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// auditLogsSqliteSchema mirrors the production migration in a
// SQLite-compatible dialect. We can't run the Postgres jsonb/char(26)
// DDL here, so the columns are TEXT/DATETIME — the repository
// behaviour the rollback test exercises is dialect-agnostic.
const auditLogsSqliteSchema = `
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
);`

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

// newAuditLogsSqliteDB spins up an in-memory SQLite-backed *gorm.DB
// with the audit_logs schema applied so the CreateInTx /
// Flush / Discard tests can exercise a real GORM tx commit/rollback.
func newAuditLogsSqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(auditLogsSqliteSchema).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

// TestNotifyingAuditWriter_CreateInTx_RolledBackTxNoDispatch is a
// regression for the phantom-notification bug. Before the fix
// CreateInTx fired the dispatcher synchronously (or via a fresh
// goroutine in Async mode) the moment the audit row was inserted —
// so a later step inside the same tx callback that returned an
// error would roll back the audit row but the webhook had already
// reached external subscribers, who then saw an event for a state
// change that never happened.
//
// The fix is to buffer the dispatch inside CreateInTx and only
// fire it once the caller invokes Flush after the surrounding tx
// has committed. On rollback the caller invokes Discard which
// drops the buffer without firing.
func TestNotifyingAuditWriter_CreateInTx_RolledBackTxNoDispatch(t *testing.T) {
	t.Parallel()

	db := newAuditLogsSqliteDB(t)
	auditRepo := audit.NewRepository(db)

	store := admin.NewInMemoryNotificationStore()
	if err := store.Create(&admin.NotificationPreference{
		TenantID: "t-1", EventType: string(audit.ActionPolicyPromoted),
		Channel: admin.NotificationChannelWebhook, Target: "https://hook.example/promoted", Enabled: true,
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	delivery := &capturingDelivery{}
	dispatcher, err := admin.NewNotificationDispatcher(store, delivery)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	naw := &admin.NotifyingAuditWriter{Inner: auditRepo, Dispatcher: dispatcher, Async: false}

	log := audit.NewAuditLog(
		"t-1", "actor-1", audit.ActionPolicyPromoted,
		"policy_draft", "01J123456789012345678901AB",
		audit.JSONMap{"channel_id": "channel-1"},
		"trace-rollback",
	)

	// Simulate the Promoter flow: open a tx, CreateInTx the audit
	// row, then return a non-nil error so the entire tx rolls back.
	// We capture the tx pointer so the assertion below mirrors the
	// promoter's flushAuditNotifications discipline.
	var capturedTx *gorm.DB
	rollbackErr := errors.New("later step failed")
	txErr := db.Transaction(func(tx *gorm.DB) error {
		capturedTx = tx
		if err := naw.CreateInTx(context.Background(), tx, log); err != nil {
			return err
		}
		// A later step (e.g. policy_versions.Insert) failed; the
		// whole tx must roll back along with the audit row.
		return rollbackErr
	})
	if !errors.Is(txErr, rollbackErr) {
		t.Fatalf("tx must surface rollback err; got %v", txErr)
	}
	naw.Discard(capturedTx)

	// The audit row must NOT have been persisted (rolled back).
	if _, err := auditRepo.Get(context.Background(), "t-1", log.ID); err == nil {
		t.Fatal("rolled-back audit row must not be readable")
	}
	// And — crucially — no notification can have been dispatched.
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if len(delivery.targets) != 0 {
		t.Fatalf("phantom-notification: rolled-back tx must not dispatch; got %v", delivery.targets)
	}
}

// TestNotifyingAuditWriter_CreateInTx_FlushAfterCommitDispatches
// is the success-path complement to the rollback regression: on a
// committed tx, Flush MUST fire the dispatcher for every buffered
// audit log. Without this assertion a regression that drops the
// Flush call would silently swallow every committed-promotion
// notification.
func TestNotifyingAuditWriter_CreateInTx_FlushAfterCommitDispatches(t *testing.T) {
	t.Parallel()

	db := newAuditLogsSqliteDB(t)
	auditRepo := audit.NewRepository(db)

	store := admin.NewInMemoryNotificationStore()
	if err := store.Create(&admin.NotificationPreference{
		TenantID: "t-1", EventType: string(audit.ActionPolicyPromoted),
		Channel: admin.NotificationChannelWebhook, Target: "https://hook.example/promoted", Enabled: true,
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	delivery := &capturingDelivery{}
	dispatcher, err := admin.NewNotificationDispatcher(store, delivery)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	naw := &admin.NotifyingAuditWriter{Inner: auditRepo, Dispatcher: dispatcher, Async: false}

	log := audit.NewAuditLog(
		"t-1", "actor-1", audit.ActionPolicyPromoted,
		"policy_draft", "01J123456789012345678901CD",
		audit.JSONMap{"channel_id": "channel-1"},
		"trace-commit",
	)

	var capturedTx *gorm.DB
	if err := db.Transaction(func(tx *gorm.DB) error {
		capturedTx = tx
		return naw.CreateInTx(context.Background(), tx, log)
	}); err != nil {
		t.Fatalf("tx must commit: %v", err)
	}

	// Before Flush the dispatcher must NOT have fired: CreateInTx
	// only buffers. This guards against a regression that fires
	// inline again.
	delivery.mu.Lock()
	preFlushDispatches := len(delivery.targets)
	delivery.mu.Unlock()
	if preFlushDispatches != 0 {
		t.Fatalf("CreateInTx must not dispatch before Flush; got %d dispatches", preFlushDispatches)
	}

	// And the audit row MUST have been persisted (committed).
	if _, err := auditRepo.Get(context.Background(), "t-1", log.ID); err != nil {
		t.Fatalf("committed audit row must be readable: %v", err)
	}

	naw.Flush(context.Background(), capturedTx)

	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if len(delivery.targets) != 1 {
		t.Fatalf("Flush must dispatch the buffered event exactly once; got %v", delivery.targets)
	}
	if delivery.targets[0] != "https://hook.example/promoted" {
		t.Fatalf("Flush dispatched to wrong target: %s", delivery.targets[0])
	}
}

// TestNotifyingAuditWriter_Flush_OnUnknownTxIsNoop guards against
// a Flush call with a tx the writer never saw (e.g. the audit
// writer doesn't implement the flush protocol or the caller
// captured a stale pointer). The call must be a safe no-op rather
// than firing an empty/stale dispatch.
func TestNotifyingAuditWriter_Flush_OnUnknownTxIsNoop(t *testing.T) {
	t.Parallel()
	naw := &admin.NotifyingAuditWriter{Inner: &round8RecordingAudit{}, Async: false}
	// nil tx must not panic.
	naw.Flush(context.Background(), nil)
	naw.Discard(nil)
}
