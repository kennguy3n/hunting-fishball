//go:build e2e

// Package e2e — notification_lifecycle_test.go — Round-9 Task 13.
//
// End-to-end coverage of the notification dispatch + retry +
// dead-letter lifecycle wired in Round-7 (Task 5) / Round-8
// (Tasks 5 + 17). This test runs against the in-memory stores
// (admin.InMemoryNotificationStore, admin.InMemoryNotificationDeliveryLog)
// so it stays docker-compose-free and exercises the same code
// paths cmd/api wires up at boot.
//
// The lifecycle under test:
//
//  1. Operator creates a Webhook subscription for `source.connected`.
//  2. An audit event of that type is persisted via NotifyingAuditWriter.
//  3. The dispatcher fires, the delivery POSTs against a fake
//     httptest server, and the delivery log gets an attempt row.
//  4. The first attempt returns 503; the dispatcher marks the row
//     `failed` and schedules next_retry_at.
//  5. The retry worker picks up the row on its next Tick and
//     re-delivers — this time we make the server return 503 again,
//     so attempt-count climbs.
//  6. After MaxAttempts cycles the worker stops re-scheduling and
//     the row stays at status=failed with no next_retry_at set
//     (dead-lettered).
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// withWebhookSubscription seeds an InMemoryNotificationStore with a
// single webhook subscription for the supplied tenant + event.
func withWebhookSubscription(t *testing.T, store *admin.InMemoryNotificationStore, tenantID, eventType, target string) {
	t.Helper()
	if err := store.Create(&admin.NotificationPreference{
		TenantID:  tenantID,
		EventType: eventType,
		Channel:   admin.NotificationChannelWebhook,
		Target:    target,
		Enabled:   true,
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// TestNotificationLifecycle_DispatchRetryDeadLetter exercises the
// full lifecycle from audit-event → dispatcher → delivery log →
// retry worker → dead-letter.
func TestNotificationLifecycle_DispatchRetryDeadLetter(t *testing.T) {
	const tenant = "tenant-e2e-notif"
	const event = "source.connected"

	// 1. Subscription store + delivery log.
	store := admin.NewInMemoryNotificationStore()
	deliveryLog := admin.NewInMemoryNotificationDeliveryLog()

	// 2. Webhook receiver that always returns 503 to exercise the
	//    retry + dead-letter path.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// 3. Dispatcher with no inner-Send retries (Backoff: []) so we
	//    can count the worker-driven retries cleanly. Sleep is a
	//    no-op so the test doesn't pause on the (unused) backoff.
	webhook := &admin.WebhookDelivery{
		Client:  srv.Client(),
		Backoff: []time.Duration{},
		Sleep:   func(time.Duration) {},
	}
	dispatcher, err := admin.NewNotificationDispatcher(store, webhook)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	dispatcher.SetDeliveryLog(deliveryLog)

	// 4. Notifying audit writer wraps a noop creator — we don't
	//    need an actual audit repo, only the fire path.
	notifying := &admin.NotifyingAuditWriter{
		Inner:      noopAuditCreator{},
		Dispatcher: dispatcher,
		Async:      false,
	}

	// 5. Seed the subscription. (Step 1 of the spec.)
	withWebhookSubscription(t, store, tenant, event, srv.URL)

	// 6. Trigger the audit event. (Step 2.) The NotifyingAuditWriter
	//    persists via Inner (noop here) and fires the dispatcher.
	ev := audit.NewAuditLog(tenant, "actor-1", audit.ActionSourceConnected, "source", "src-42", audit.JSONMap{}, "")
	if err := notifying.Create(context.Background(), ev); err != nil {
		t.Fatalf("audit create: %v", err)
	}

	// 7. The delivery log must now have exactly one failed attempt
	//    with the retryable 503 status. (Step 3.)
	rows, _ := deliveryLog.List(context.Background(), tenant, 100)
	if len(rows) != 1 {
		t.Fatalf("expected 1 delivery attempt; got %d", len(rows))
	}
	first := rows[0]
	if first.Status != admin.NotificationDeliveryStatusFailed {
		t.Fatalf("first attempt: status = %s, want failed", first.Status)
	}
	if first.ResponseCode != http.StatusServiceUnavailable {
		t.Fatalf("first attempt: code = %d, want 503", first.ResponseCode)
	}
	if first.NextRetryAt == nil {
		t.Fatal("first attempt: next_retry_at must be scheduled for retryable 503")
	}

	// 8. Backdate next_retry_at so the worker picks it up
	//    immediately — the production wiring polls on a Ticker
	//    so we need to fast-forward.
	past := time.Now().UTC().Add(-time.Minute)
	first.NextRetryAt = &past
	if err := deliveryLog.UpdateAttempt(context.Background(), first); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// 9. Retry worker with MaxAttempts=3 (Step 4 + 5).
	worker, err := admin.NewNotificationRetryWorker(admin.NotificationRetryWorkerConfig{
		Store:       deliveryLog,
		Delivery:    webhook,
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("retry worker: %v", err)
	}

	// 10. Tick the worker; the 503 again keeps the row in failed
	//     with bumped attempt and a fresh next_retry_at.
	processed := worker.Tick(context.Background())
	if processed != 1 {
		t.Fatalf("worker tick 1: processed = %d, want 1", processed)
	}
	rows, _ = deliveryLog.List(context.Background(), tenant, 100)
	if got := rows[0].Attempt; got != 2 {
		t.Fatalf("after retry 1: attempt = %d, want 2", got)
	}
	// retryOne marks intermediate retries as `pending` and only
	// switches back to `failed` on dead-letter — see
	// notification_retry_worker.go.
	if rows[0].Status != admin.NotificationDeliveryStatusPending {
		t.Fatalf("after retry 1: status = %s, want pending", rows[0].Status)
	}

	// 11. Drive the worker to MaxAttempts to force the dead-letter
	//     path. (Step 5.) Backdate between cycles since the worker
	//     re-schedules next_retry_at into the future on each
	//     failure.
	for cycle := 0; cycle < 3; cycle++ {
		rows, _ = deliveryLog.List(context.Background(), tenant, 100)
		if rows[0].NextRetryAt == nil {
			break // dead-lettered before we hit MaxAttempts
		}
		past = time.Now().UTC().Add(-time.Minute)
		rows[0].NextRetryAt = &past
		_ = deliveryLog.UpdateAttempt(context.Background(), rows[0])
		_ = worker.Tick(context.Background())
	}

	// 12. Final state: status=failed, NextRetryAt=nil so the worker
	//     leaves the row alone (dead-letter). hits > 1 confirms
	//     the worker re-issued the request.
	rows, _ = deliveryLog.List(context.Background(), tenant, 100)
	if rows[0].Status != admin.NotificationDeliveryStatusFailed {
		t.Fatalf("final status = %s, want failed", rows[0].Status)
	}
	if rows[0].NextRetryAt != nil {
		t.Fatalf("dead-letter expected: NextRetryAt = %v", rows[0].NextRetryAt)
	}
	if hits.Load() < 2 {
		t.Fatalf("expected webhook to be hit at least twice; got %d", hits.Load())
	}
	if rows[0].Attempt < 3 {
		t.Fatalf("expected at least 3 attempts; got %d", rows[0].Attempt)
	}
}

// TestNotificationLifecycle_HappyPathDeliversFirstTry checks the
// dispatcher records a delivered attempt with no next_retry_at
// when the webhook returns 200.
func TestNotificationLifecycle_HappyPathDeliversFirstTry(t *testing.T) {
	const tenant = "tenant-e2e-notif-ok"
	const event = "source.connected"

	store := admin.NewInMemoryNotificationStore()
	deliveryLog := admin.NewInMemoryNotificationDeliveryLog()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	webhook := &admin.WebhookDelivery{Client: srv.Client(), Backoff: []time.Duration{}, Sleep: func(time.Duration) {}}
	dispatcher, _ := admin.NewNotificationDispatcher(store, webhook)
	dispatcher.SetDeliveryLog(deliveryLog)

	notifying := &admin.NotifyingAuditWriter{
		Inner:      noopAuditCreator{},
		Dispatcher: dispatcher,
	}

	withWebhookSubscription(t, store, tenant, event, srv.URL)

	ev := audit.NewAuditLog(tenant, "actor-1", audit.ActionSourceConnected, "source", "src-1", audit.JSONMap{}, "")
	if err := notifying.Create(context.Background(), ev); err != nil {
		t.Fatalf("create: %v", err)
	}
	rows, _ := deliveryLog.List(context.Background(), tenant, 100)
	if len(rows) != 1 {
		t.Fatalf("expected 1 attempt; got %d", len(rows))
	}
	if rows[0].Status != admin.NotificationDeliveryStatusDelivered {
		t.Fatalf("status = %s, want delivered", rows[0].Status)
	}
	if rows[0].NextRetryAt != nil {
		t.Fatalf("happy path must not schedule retry; got %v", rows[0].NextRetryAt)
	}
}

// noopAuditCreator satisfies admin.NotifyingAuditWriter.Inner. We
// don't need a real audit repo because Round-9 Task 13 only cares
// about the dispatcher fan-out.
type noopAuditCreator struct{}

func (noopAuditCreator) Create(_ context.Context, _ *audit.AuditLog) error { return nil }

// jsonMust is a defensive guard against test typos in payload
// construction.
func jsonMust(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

var _ = jsonMust // referenced only by future expansions
