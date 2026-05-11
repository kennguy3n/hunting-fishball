// notification_retry_worker.go — Round-8 Task 17.
//
// The dispatcher records failed webhook deliveries in
// notification_delivery_log with a non-nil next_retry_at column
// when the failure is retryable (5xx or transport-level). The
// retry worker periodically scans the log for rows whose
// next_retry_at has passed, attempts redelivery, and updates the
// row. After MaxRetryAttempts the row is marked failed and
// next_retry_at is cleared (effectively dead-lettered).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// NotificationDeliveryLogRetryStore is the subset of the delivery
// log surface the retry worker needs. It's separate from
// NotificationDeliveryLog so the in-memory test fake does not
// need to implement the GORM-only Pending/Update helpers.
type NotificationDeliveryLogRetryStore interface {
	NotificationDeliveryLog
	ListPending(ctx context.Context, now time.Time, limit int) ([]*NotificationDeliveryAttempt, error)
	UpdateAttempt(ctx context.Context, a *NotificationDeliveryAttempt) error
}

// NotificationRetryWorker re-attempts failed webhook deliveries.
type NotificationRetryWorker struct {
	store       NotificationDeliveryLogRetryStore
	delivery    NotificationDelivery
	logger      func(string, ...any)
	maxAttempts int
}

// NotificationRetryWorkerConfig wires the worker.
type NotificationRetryWorkerConfig struct {
	Store       NotificationDeliveryLogRetryStore
	Delivery    NotificationDelivery
	Logger      func(string, ...any)
	MaxAttempts int
}

// DefaultMaxRetryAttempts caps the retry worker's per-row attempts.
const DefaultMaxRetryAttempts = 5

// NewNotificationRetryWorker validates inputs and constructs the
// worker.
func NewNotificationRetryWorker(cfg NotificationRetryWorkerConfig) (*NotificationRetryWorker, error) {
	if cfg.Store == nil {
		return nil, errors.New("notification_retry: nil store")
	}
	if cfg.Delivery == nil {
		return nil, errors.New("notification_retry: nil delivery")
	}
	maxA := cfg.MaxAttempts
	if maxA <= 0 {
		maxA = DefaultMaxRetryAttempts
	}
	logger := cfg.Logger
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &NotificationRetryWorker{
		store: cfg.Store, delivery: cfg.Delivery,
		logger: logger, maxAttempts: maxA,
	}, nil
}

// Tick processes one batch of pending retries. Callers wire this
// behind a time.Ticker. Returns the number of rows processed.
func (w *NotificationRetryWorker) Tick(ctx context.Context) int {
	rows, err := w.store.ListPending(ctx, time.Now().UTC(), 100)
	if err != nil {
		w.logger("notification_retry: list pending", "err", err)
		return 0
	}
	processed := 0
	for _, r := range rows {
		w.retryOne(ctx, r)
		processed++
	}
	return processed
}

func (w *NotificationRetryWorker) retryOne(ctx context.Context, a *NotificationDeliveryAttempt) {
	// Reconstruct the body from the persisted payload + event_type.
	body, err := json.Marshal(map[string]any{
		"tenant_id": a.TenantID, "event_type": a.EventType,
		"payload": a.Payload, "ts": time.Now().UTC(),
	})
	if err != nil {
		w.logger("notification_retry: marshal", "id", a.ID, "err", err)
		return
	}
	result, derr := w.delivery.Send(ctx, a.Target, a.Channel, body)
	// Attempt counts worker cycles (one per Tick on this row),
	// not inner HTTP retries inside Send. Accumulating
	// result.Attempts here would conflate the two counters and
	// push the row past MaxAttempts after a single worker cycle
	// on a flaky endpoint, defeating the per-row retry budget.
	a.Attempt++
	a.ResponseCode = result.StatusCode
	if derr == nil {
		a.Status = NotificationDeliveryStatusDelivered
		a.ErrorMessage = ""
		a.NextRetryAt = nil
		_ = w.store.UpdateAttempt(ctx, a)
		return
	}
	a.ErrorMessage = derr.Error()
	if !isRetryableResponseCode(result.StatusCode) || a.Attempt >= w.maxAttempts {
		// Dead-letter: keep status=failed and clear next_retry_at
		// so we stop scheduling further retries.
		a.Status = NotificationDeliveryStatusFailed
		a.NextRetryAt = nil
	} else {
		// Schedule the next retry with simple linear backoff
		// scaled by attempt count.
		next := time.Now().UTC().Add(time.Duration(a.Attempt) * time.Minute)
		a.NextRetryAt = &next
		a.Status = NotificationDeliveryStatusPending
	}
	if uerr := w.store.UpdateAttempt(ctx, a); uerr != nil {
		w.logger("notification_retry: update", "id", a.ID, "err", uerr)
	}
}

// ---- GORM-backed retry-store implementation ----

// ListPending returns notification_delivery_log rows whose
// next_retry_at is non-null and not in the future.
func (s *NotificationDeliveryLogGORM) ListPending(ctx context.Context, now time.Time, limit int) ([]*NotificationDeliveryAttempt, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	out := []*NotificationDeliveryAttempt{}
	err := s.db.WithContext(ctx).
		Where("next_retry_at IS NOT NULL AND next_retry_at <= ?", now).
		Order("next_retry_at ASC").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// UpdateAttempt persists changes to a single attempt row. The PK
// is the row's ID; this is upsert-by-PK.
func (s *NotificationDeliveryLogGORM) UpdateAttempt(ctx context.Context, a *NotificationDeliveryAttempt) error {
	if a == nil || a.ID == "" {
		return errors.New("notification_log: missing id")
	}
	a.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).
		Model(&NotificationDeliveryAttempt{}).
		Where("id = ?", a.ID).
		Updates(map[string]any{
			"status":        a.Status,
			"attempt":       a.Attempt,
			"response_code": a.ResponseCode,
			"error_message": a.ErrorMessage,
			"next_retry_at": a.NextRetryAt,
			"updated_at":    a.UpdatedAt,
		}).Error
}

// ---- assertion: in-memory log can also satisfy the retry store ----

// Both implementations satisfy NotificationDeliveryLogRetryStore.
var _ NotificationDeliveryLogRetryStore = (*NotificationDeliveryLogGORM)(nil)

// ListPending walks the in-memory entries and returns those ready
// for retry. Used by unit tests so the retry worker can be tested
// without a database.
func (s *InMemoryNotificationDeliveryLog) ListPending(_ context.Context, now time.Time, limit int) ([]*NotificationDeliveryAttempt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*NotificationDeliveryAttempt{}
	for _, e := range s.entries {
		if e.NextRetryAt == nil {
			continue
		}
		if e.NextRetryAt.After(now) {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// UpdateAttempt replaces the in-memory entry whose ID matches.
func (s *InMemoryNotificationDeliveryLog) UpdateAttempt(_ context.Context, a *NotificationDeliveryAttempt) error {
	if a == nil || a.ID == "" {
		return errors.New("notification_log: missing id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if e.ID == a.ID {
			cp := *a
			cp.UpdatedAt = time.Now().UTC()
			s.entries[i] = &cp
			return nil
		}
	}
	return errors.New("notification_log: not found")
}

var _ NotificationDeliveryLogRetryStore = (*InMemoryNotificationDeliveryLog)(nil)

// guard against unused import warnings when gorm is absent.
var _ = gorm.ErrRecordNotFound
