package admin_test

// notification_retry_worker_test.go — Round-8 Task 17.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// scriptedDelivery returns canned outcomes in order. The Nth call
// returns scripts[N-1]; calls past the end repeat the last entry.
type scriptedDelivery struct {
	mu      sync.Mutex
	scripts []scriptedOutcome
	calls   atomic.Int32
}

type scriptedOutcome struct {
	result admin.DeliveryResult
	err    error
}

func (s *scriptedDelivery) Send(_ context.Context, _ string, _ admin.NotificationChannel, _ []byte) (admin.DeliveryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := int(s.calls.Add(1)) - 1
	if idx >= len(s.scripts) {
		idx = len(s.scripts) - 1
	}
	o := s.scripts[idx]
	return o.result, o.err
}

func TestNotificationRetryWorker_RetriesPendingAttempt_Success(t *testing.T) {
	t.Parallel()
	log := admin.NewInMemoryNotificationDeliveryLog()
	past := time.Now().UTC().Add(-time.Minute)
	if err := log.Append(context.Background(), &admin.NotificationDeliveryAttempt{
		TenantID:     "t-1",
		PreferenceID: "p-1",
		EventType:    "source.connected",
		Channel:      admin.NotificationChannelWebhook,
		Target:       "https://example.com/hook",
		Payload:      admin.JSONMap{"k": "v"},
		Status:       admin.NotificationDeliveryStatusFailed,
		Attempt:      1,
		ResponseCode: 503,
		ErrorMessage: "upstream blew up",
		NextRetryAt:  &past,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	delivery := &scriptedDelivery{scripts: []scriptedOutcome{
		{result: admin.DeliveryResult{Attempts: 1, StatusCode: 200}, err: nil},
	}}
	w, err := admin.NewNotificationRetryWorker(admin.NotificationRetryWorkerConfig{
		Store:    log,
		Delivery: delivery,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	n := w.Tick(context.Background())
	if n != 1 {
		t.Fatalf("expected 1 retry; got %d", n)
	}
	rows, _ := log.List(context.Background(), "t-1", 100)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row; got %d", len(rows))
	}
	if rows[0].Status != admin.NotificationDeliveryStatusDelivered {
		t.Fatalf("expected delivered; got %s", rows[0].Status)
	}
	if rows[0].NextRetryAt != nil {
		t.Fatalf("expected next_retry_at cleared")
	}
}

func TestNotificationRetryWorker_DeadLettersAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	log := admin.NewInMemoryNotificationDeliveryLog()
	past := time.Now().UTC().Add(-time.Minute)
	if err := log.Append(context.Background(), &admin.NotificationDeliveryAttempt{
		TenantID: "t-1", PreferenceID: "p-1",
		EventType:   "source.connected",
		Channel:     admin.NotificationChannelWebhook,
		Target:      "https://example.com/hook",
		Payload:     admin.JSONMap{},
		Status:      admin.NotificationDeliveryStatusFailed,
		Attempt:     5, // already at MaxAttempts
		NextRetryAt: &past,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	delivery := &scriptedDelivery{scripts: []scriptedOutcome{
		{result: admin.DeliveryResult{Attempts: 1, StatusCode: 503}, err: errors.New("still bad")},
	}}
	w, _ := admin.NewNotificationRetryWorker(admin.NotificationRetryWorkerConfig{
		Store: log, Delivery: delivery, MaxAttempts: 5,
	})
	w.Tick(context.Background())
	rows, _ := log.List(context.Background(), "t-1", 100)
	if rows[0].Status != admin.NotificationDeliveryStatusFailed {
		t.Fatalf("expected failed; got %s", rows[0].Status)
	}
	if rows[0].NextRetryAt != nil {
		t.Fatalf("expected next_retry_at cleared after dead-letter")
	}
}

// TestNotificationRetryWorker_AttemptIncrementsByWorkerCycle is a
// regression for the Attempt-counter bug where the worker added
// result.Attempts (inner HTTP retries from Send) to the row's
// Attempt counter every Tick. With a flaky 5xx endpoint and a
// 4-hop backoff inside Send, that previously pushed Attempt from 1
// to 5 in a single worker cycle and dead-lettered the row
// immediately. The fix is to increment Attempt by exactly one per
// worker cycle so the per-row retry budget actually spans the
// documented MaxAttempts cycles.
func TestNotificationRetryWorker_AttemptIncrementsByWorkerCycle(t *testing.T) {
	t.Parallel()
	log := admin.NewInMemoryNotificationDeliveryLog()
	past := time.Now().UTC().Add(-time.Minute)
	if err := log.Append(context.Background(), &admin.NotificationDeliveryAttempt{
		TenantID: "t-1", PreferenceID: "p-1",
		EventType: "source.connected",
		Channel:   admin.NotificationChannelWebhook,
		Target:    "https://example.com/hook",
		Payload:   admin.JSONMap{},
		Status:    admin.NotificationDeliveryStatusFailed,
		Attempt:   1, NextRetryAt: &past,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Every Send returns a 4-hop retry burst that still fails;
	// before the fix this would jump Attempt from 1 to 5 in one
	// Tick and dead-letter immediately. After the fix the row
	// must remain pending and Attempt must be exactly 2.
	delivery := &scriptedDelivery{scripts: []scriptedOutcome{
		{result: admin.DeliveryResult{Attempts: 4, StatusCode: 503}, err: errors.New("flaky 5xx")},
	}}
	w, _ := admin.NewNotificationRetryWorker(admin.NotificationRetryWorkerConfig{
		Store: log, Delivery: delivery, MaxAttempts: 5,
	})
	w.Tick(context.Background())
	rows, _ := log.List(context.Background(), "t-1", 100)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row; got %d", len(rows))
	}
	if rows[0].Attempt != 2 {
		t.Fatalf("expected Attempt=2 after one worker cycle; got %d", rows[0].Attempt)
	}
	if rows[0].Status != admin.NotificationDeliveryStatusPending {
		t.Fatalf("expected pending (still under MaxAttempts); got %s", rows[0].Status)
	}
	if rows[0].NextRetryAt == nil {
		t.Fatalf("expected next_retry_at scheduled (still under MaxAttempts)")
	}
}

func TestNotificationRetryWorker_LeavesFutureRowsAlone(t *testing.T) {
	t.Parallel()
	log := admin.NewInMemoryNotificationDeliveryLog()
	future := time.Now().UTC().Add(time.Hour)
	_ = log.Append(context.Background(), &admin.NotificationDeliveryAttempt{
		TenantID: "t-1", PreferenceID: "p-1",
		EventType: "x", Channel: admin.NotificationChannelWebhook,
		Target: "https://example.com/", Payload: admin.JSONMap{},
		Status:  admin.NotificationDeliveryStatusFailed,
		Attempt: 1, NextRetryAt: &future,
	})
	delivery := &scriptedDelivery{scripts: []scriptedOutcome{
		{result: admin.DeliveryResult{Attempts: 1, StatusCode: 200}, err: nil},
	}}
	w, _ := admin.NewNotificationRetryWorker(admin.NotificationRetryWorkerConfig{
		Store: log, Delivery: delivery,
	})
	n := w.Tick(context.Background())
	if n != 0 {
		t.Fatalf("expected 0 retries (future row); got %d", n)
	}
	if delivery.calls.Load() != 0 {
		t.Fatalf("delivery should not have been called: %d", delivery.calls.Load())
	}
}
