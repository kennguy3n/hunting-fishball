// dlq_age_monitor.go — Round-13 Task 4.
//
// A long-tail DLQ row almost always means a poison message a human
// has not yet acknowledged. The auto-replay worker (Round-12 Task 6)
// recycles transient failures, but anything that's been on the DLQ
// for hours is by definition NOT transient — it sits there waiting
// for an operator to either replay it, fix the source data, or
// purge it.
//
// DLQAgeMonitor is a tiny background goroutine that polls the DLQ
// store at a fixed interval and publishes
// `context_engine_dlq_oldest_message_age_seconds` so the operator
// dashboard + the DLQAgeHigh alert in deploy/alerts.yaml can fire.
package pipeline

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// DLQOldestLister returns the timestamp of the oldest unresolved
// DLQ row (replayed_at IS NULL). When no such row exists the
// implementation returns the zero time + ErrNoUnresolvedRows.
type DLQOldestLister interface {
	OldestUnresolved(ctx context.Context) (time.Time, error)
}

// ErrNoUnresolvedRows signals to DLQAgeMonitor that the DLQ has no
// pending rows — the gauge is reset to 0 on this signal.
var ErrNoUnresolvedRows = errors.New("dlq: no unresolved rows")

// OldestUnresolved adds an OldestUnresolved method to DLQStoreGORM.
// The method is here (instead of dlq_consumer.go) so the DLQ age
// monitor is a self-contained file the e2e tests can examine.
func (s *DLQStoreGORM) OldestUnresolved(ctx context.Context) (time.Time, error) {
	var row DLQMessage
	res := s.db.WithContext(ctx).
		Where("replayed_at IS NULL").
		Order("failed_at ASC").
		Limit(1).
		Find(&row)
	if res.Error != nil {
		return time.Time{}, res.Error
	}
	if res.RowsAffected == 0 {
		return time.Time{}, ErrNoUnresolvedRows
	}
	return row.FailedAt, nil
}

// DLQAgeMonitorConfig configures the background goroutine.
type DLQAgeMonitorConfig struct {
	Lister DLQOldestLister
	// Interval defaults to 1 minute when zero.
	Interval time.Duration
	// nowFn is overridable in tests so the gauge value is
	// deterministic.
	NowFn func() time.Time
}

// DLQAgeMonitor is the background poller.
type DLQAgeMonitor struct {
	cfg DLQAgeMonitorConfig
}

// NewDLQAgeMonitor wires the poller.
func NewDLQAgeMonitor(cfg DLQAgeMonitorConfig) (*DLQAgeMonitor, error) {
	if cfg.Lister == nil {
		return nil, errors.New("dlq age monitor: Lister required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.NowFn == nil {
		cfg.NowFn = func() time.Time { return time.Now().UTC() }
	}
	return &DLQAgeMonitor{cfg: cfg}, nil
}

// Run starts the poll loop. The loop exits cleanly on ctx.Done.
func (m *DLQAgeMonitor) Run(ctx context.Context) error {
	m.observe(ctx)
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.observe(ctx)
		}
	}
}

// observe runs one poll cycle and publishes the gauge. Failures
// to read the DLQ leave the previous gauge value in place so a
// transient db blip doesn't flap the alert.
func (m *DLQAgeMonitor) observe(ctx context.Context) {
	oldest, err := m.cfg.Lister.OldestUnresolved(ctx)
	if err != nil {
		if errors.Is(err, ErrNoUnresolvedRows) || errors.Is(err, gorm.ErrRecordNotFound) {
			observability.DLQOldestMessageAgeSeconds.Set(0)
		}
		return
	}
	if oldest.IsZero() {
		observability.DLQOldestMessageAgeSeconds.Set(0)
		return
	}
	age := m.cfg.NowFn().Sub(oldest)
	if age < 0 {
		age = 0
	}
	observability.DLQOldestMessageAgeSeconds.Set(age.Seconds())
}
