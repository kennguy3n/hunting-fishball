// Round-12 Task 6 — DLQ auto-replay worker with capped exponential
// backoff.
//
// The worker is a background goroutine that:
//
//  1. Periodically lists DLQ rows where:
//     attempt_count < MaxAutoRetries (default 3)  AND
//     replayed_at IS NULL                           AND
//     failed_at + backoffFor(attempt_count) <= now()
//
//     The backoff schedule is [1m, 5m, 30m] indexed by
//     attempt_count, so the worker re-emits at 1m, then 5m, then
//     30m after each successive failure — fast enough that
//     transient outages clear without operator action, slow
//     enough that a flapping connector doesn't hammer Kafka.
//
//  2. Re-emits each eligible row via the existing pipeline.Replayer
//     so the bookkeeping (MarkReplayed → BumpAttemptCount) is
//     identical to the manual admin path.
//
//  3. Increments context_engine_dlq_auto_replays_total
//     {outcome="success"|"failure"|"skip_max_retries"} on each
//     decision.
//
// Wiring is opt-in behind CONTEXT_ENGINE_DLQ_AUTO_REPLAY=true; the
// worker is constructed in cmd/ingest only when that env var is set.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// DefaultAutoReplayMaxRetries caps how many times the auto-replay
// worker will re-emit a single DLQ row before falling back to the
// admin's manual replay surface. The admin handler's MaxAttempts
// (default 5) is intentionally larger so an operator can still
// force a replay after the auto-replayer gives up.
const DefaultAutoReplayMaxRetries = 3

// AutoReplayBackoff is the schedule applied to attempt_count.
// attempt_count==0 → 1 minute after failed_at; attempt_count==1 → 5
// minutes; attempt_count==2 → 30 minutes. The slice is len ==
// DefaultAutoReplayMaxRetries so the worker never indexes past the
// retry budget; callers must use BackoffFor() rather than indexing
// directly so an over-budget attempt_count returns a sentinel.
var AutoReplayBackoff = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
}

// BackoffFor returns the wait duration before re-emitting a row
// whose current attempt_count is n. Returns (0, false) once n
// exceeds the retry budget so callers can treat the row as
// permanently abandoned by the worker.
func BackoffFor(n int) (time.Duration, bool) {
	if n < 0 || n >= len(AutoReplayBackoff) {
		return 0, false
	}
	return AutoReplayBackoff[n], true
}

// AutoReplayStore is the narrow read seam the worker needs. We
// don't reuse DLQListFilter.TenantID because the worker scans every
// tenant in one pass — DLQ replay is an operator-level recovery
// path, not a tenant-facing API.
type AutoReplayStore interface {
	ListAutoReplayable(ctx context.Context, now time.Time, limit int) ([]DLQMessage, error)
	Get(ctx context.Context, tenantID, id string) (*DLQMessage, error)
}

// AutoReplayer is the narrow re-emit seam. The production wiring
// passes a *Replayer; tests pass a recording fake.
type AutoReplayer interface {
	Replay(ctx context.Context, tenantID, id, topic string, force bool) error
	MaxAttempts() int
}

// DLQAutoReplayConfig configures a DLQAutoReplayer.
type DLQAutoReplayConfig struct {
	Store    AutoReplayStore
	Replayer AutoReplayer
	Logger   *slog.Logger

	// Interval is the sleep between scans. Defaults to 30s. The
	// worker also runs once immediately on Start so the first
	// eligible row clears in <Interval seconds rather than waiting
	// a full tick.
	Interval time.Duration

	// BatchSize caps each scan. Defaults to 100; large enough to
	// drain a healthy queue quickly, small enough that one tick
	// can't tie up the Kafka producer.
	BatchSize int

	// MaxAutoRetries is the per-row retry budget. Defaults to
	// DefaultAutoReplayMaxRetries.
	MaxAutoRetries int

	// Now lets tests inject a clock. Defaults to time.Now.UTC.
	Now func() time.Time
}

// DLQAutoReplayer is the background worker.
type DLQAutoReplayer struct {
	cfg DLQAutoReplayConfig
}

// NewDLQAutoReplayer validates cfg and returns a worker.
func NewDLQAutoReplayer(cfg DLQAutoReplayConfig) (*DLQAutoReplayer, error) {
	if cfg.Store == nil {
		return nil, errors.New("dlq auto-replay: nil Store")
	}
	if cfg.Replayer == nil {
		return nil, errors.New("dlq auto-replay: nil Replayer")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxAutoRetries <= 0 {
		cfg.MaxAutoRetries = DefaultAutoReplayMaxRetries
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &DLQAutoReplayer{cfg: cfg}, nil
}

// Run executes Tick on a ticker until ctx is cancelled. Errors
// from a single tick are logged but never propagate so a transient
// DB blip doesn't kill the worker.
func (w *DLQAutoReplayer) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.cfg.Logger.Warn("dlq auto-replay: tick", slog.String("error", err.Error()))
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("dlq auto-replay: tick", slog.String("error", err.Error()))
			}
		}
	}
}

// Tick runs one scan + replay pass. Exposed for tests.
func (w *DLQAutoReplayer) Tick(ctx context.Context) error {
	now := w.cfg.Now()
	rows, err := w.cfg.Store.ListAutoReplayable(ctx, now, w.cfg.BatchSize)
	if err != nil {
		return err
	}
	for i := range rows {
		row := rows[i]
		if row.AttemptCount >= w.cfg.MaxAutoRetries {
			observability.DLQAutoReplaysTotal.WithLabelValues("skip_max_retries").Inc()
			continue
		}
		wait, ok := BackoffFor(row.AttemptCount)
		if !ok {
			observability.DLQAutoReplaysTotal.WithLabelValues("skip_max_retries").Inc()
			continue
		}
		if now.Before(row.FailedAt.Add(wait)) {
			// Not yet eligible — the SQL filter is approximate
			// (it picks up rows whose newest failure was long
			// enough ago), but we re-check here for safety.
			continue
		}
		if err := w.cfg.Replayer.Replay(ctx, row.TenantID, row.ID, row.OriginalTopic, false); err != nil {
			observability.DLQAutoReplaysTotal.WithLabelValues("failure").Inc()
			w.cfg.Logger.Warn("dlq auto-replay: replay failed",
				slog.String("dlq_id", row.ID),
				slog.String("tenant_id", row.TenantID),
				slog.Int("attempt_count", row.AttemptCount),
				slog.String("error", err.Error()),
			)
			continue
		}
		observability.DLQAutoReplaysTotal.WithLabelValues("success").Inc()
	}
	return nil
}

// ListAutoReplayable is the gorm-backed read for the worker. It is
// a method on DLQStoreGORM so the worker can keep its narrow
// AutoReplayStore interface clean of tenant filtering.
func (s *DLQStoreGORM) ListAutoReplayable(ctx context.Context, now time.Time, limit int) ([]DLQMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	// The SQL filter is a coarse pre-cut — it picks up every row
	// whose attempt_count is still in budget and whose failure
	// happened more than the minimum backoff window ago. The
	// worker's in-memory check (now.Before(failed_at + backoff))
	// applies the exact per-attempt timing.
	cutoff := now.Add(-AutoReplayBackoff[0])
	var rows []DLQMessage
	q := s.db.WithContext(ctx).
		Where("replayed_at IS NULL").
		Where("attempt_count < ?", DefaultAutoReplayMaxRetries).
		Where("failed_at <= ?", cutoff).
		Order("failed_at ASC").
		Limit(limit)
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Compile-time assertion that *DLQStoreGORM satisfies the worker's
// narrow read seam. Forces the build to fail if the method
// signature drifts.
var _ AutoReplayStore = (*DLQStoreGORM)(nil)

// Compile-time assertion that *Replayer satisfies AutoReplayer.
var _ AutoReplayer = (*Replayer)(nil)

// Suppress unused import warning from gorm when the file is
// trimmed during a refactor.
var _ = (*gorm.DB)(nil)
