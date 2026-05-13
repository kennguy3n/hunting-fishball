package admin

// source_auto_pause.go — Round-19 Task 24.
//
// Per-connector health degradation detection. The detector tracks
// (success, error) counters per source over a sliding window. When
// the error rate exceeds a configurable threshold over the window
// the detector auto-pauses the source and emits a
// source.auto_paused audit event. The companion Prometheus alert
// (deploy/alerts.yaml: SourceAutopaused) routes operators to the
// affected source.
//
// Design constraints:
//
//   - Hot-path safe: Record is a pair of integer increments under
//     a per-source mutex. No I/O, no allocations on the success
//     path.
//   - Idempotent pause: PauseSource is responsible for de-duping
//     a "source already paused" call — the detector simply
//     records the audit event once per breach (window cooldown).
//   - Sliding window: the window is a bucketed ring. Each bucket
//     ages out at WindowDuration / WindowBuckets. The error rate
//     is computed across every live bucket.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// SourcePauser is the narrow capability the auto-pause detector
// needs from the admin surface. The handler-level source store
// implements it directly.
type SourcePauser interface {
	PauseSource(ctx context.Context, tenantID, sourceID string) error
}

// AutoPauseAuditWriter is the narrow capability used to persist a
// source.auto_paused event. The audit.Repository satisfies it.
type AutoPauseAuditWriter interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// SourceAutoPauseConfig configures the detector.
type SourceAutoPauseConfig struct {
	// WindowDuration is the trailing window over which the error
	// rate is evaluated. Defaults to 10 minutes when zero.
	WindowDuration time.Duration

	// WindowBuckets is the number of sub-buckets the sliding
	// window is split into. Higher values give finer granularity
	// at the cost of more memory. Defaults to 10 when zero.
	WindowBuckets int

	// ErrorRateThreshold is the (errors / total) ratio at which a
	// source is auto-paused. 0 < threshold < 1. Defaults to 0.5
	// when zero.
	ErrorRateThreshold float64

	// MinSampleSize is the minimum number of observations within
	// the window before the threshold is evaluated. Avoids
	// pausing a source on a single transient failure. Defaults
	// to 20 when zero.
	MinSampleSize int

	// PauseCooldown is the minimum interval between successive
	// auto-pause emissions for the same source. Defaults to the
	// window duration when zero.
	PauseCooldown time.Duration

	// ConsecutiveFailureThreshold is the Round-24 Task 12
	// addition: when set to a positive value, the detector
	// pauses a source after this many consecutive failures
	// regardless of the sliding-window error-rate. Operators
	// who want a fast-stop on a hard-down upstream set
	// CONTEXT_ENGINE_SOURCE_AUTO_PAUSE_THRESHOLD to a small
	// positive integer; the existing rate-based trigger keeps
	// catching slow-burn degradations. Zero disables the
	// consecutive-failure trigger entirely.
	ConsecutiveFailureThreshold int

	// Now is the wall-clock provider, swappable for tests.
	// Defaults to time.Now when nil.
	Now func() time.Time
}

// SourceAutoPauser monitors per-source error rates and triggers
// pause + audit when a source breaches the configured threshold.
// Round-19 Task 24.
type SourceAutoPauser struct {
	cfg    SourceAutoPauseConfig
	pauser SourcePauser
	writer AutoPauseAuditWriter
	mu     sync.Mutex
	state  map[autoPauseKey]*autoPauseState
}

type autoPauseKey struct {
	tenantID string
	sourceID string
}

type autoPauseState struct {
	buckets    []autoPauseBucket
	cursor     int
	cursorTime time.Time
	lastPaused time.Time
	// consecutiveFailures counts the run-length of contiguous
	// ok=false observations. Resets on the first ok=true.
	consecutiveFailures int
	// pendingPause guards against concurrent Record callers fanning
	// out parallel PauseSource attempts while a pause is in flight.
	// It is set under p.mu before the pause is attempted outside
	// the lock and cleared once the attempt commits (success) or
	// rolls back (failure).
	pendingPause bool
}

type autoPauseBucket struct {
	total  int64
	errors int64
}

// NewSourceAutoPauser validates cfg and returns a *SourceAutoPauser.
func NewSourceAutoPauser(cfg SourceAutoPauseConfig, pauser SourcePauser, writer AutoPauseAuditWriter) (*SourceAutoPauser, error) {
	if pauser == nil {
		return nil, errors.New("source-auto-pause: nil pauser")
	}
	if writer == nil {
		return nil, errors.New("source-auto-pause: nil audit writer")
	}
	if cfg.WindowDuration <= 0 {
		cfg.WindowDuration = 10 * time.Minute
	}
	if cfg.WindowBuckets <= 0 {
		cfg.WindowBuckets = 10
	}
	if cfg.ErrorRateThreshold <= 0 || cfg.ErrorRateThreshold >= 1 {
		cfg.ErrorRateThreshold = 0.5
	}
	if cfg.MinSampleSize <= 0 {
		cfg.MinSampleSize = 20
	}
	if cfg.PauseCooldown <= 0 {
		cfg.PauseCooldown = cfg.WindowDuration
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &SourceAutoPauser{
		cfg:    cfg,
		pauser: pauser,
		writer: writer,
		state:  map[autoPauseKey]*autoPauseState{},
	}, nil
}

// Record observes one sync attempt. ok=true signals success;
// ok=false signals a failure that counts toward the error rate.
// When the post-record window breaches the configured threshold
// and the cooldown has elapsed, the source is auto-paused.
func (p *SourceAutoPauser) Record(ctx context.Context, tenantID, sourceID string, ok bool) error {
	if tenantID == "" || sourceID == "" {
		return errors.New("source-auto-pause: missing tenant/source id")
	}
	key := autoPauseKey{tenantID: tenantID, sourceID: sourceID}
	now := p.cfg.Now()

	p.mu.Lock()
	state, exists := p.state[key]
	if !exists {
		state = &autoPauseState{
			buckets:    make([]autoPauseBucket, p.cfg.WindowBuckets),
			cursor:     0,
			cursorTime: now,
		}
		p.state[key] = state
	}
	p.advanceLocked(state, now)
	b := &state.buckets[state.cursor]
	b.total++
	if !ok {
		b.errors++
		state.consecutiveFailures++
	} else {
		state.consecutiveFailures = 0
	}
	total, errs := p.windowTotalsLocked(state)
	rateBreach := total >= int64(p.cfg.MinSampleSize) && float64(errs)/float64(total) >= p.cfg.ErrorRateThreshold
	consecBreach := p.cfg.ConsecutiveFailureThreshold > 0 && state.consecutiveFailures >= p.cfg.ConsecutiveFailureThreshold
	// Snapshot consecutiveFailures inside the lock so the audit
	// metadata below cannot race with concurrent Record callers
	// that may mutate the field after we release p.mu.
	consecutiveFailures := state.consecutiveFailures
	breach := rateBreach || consecBreach
	cooled := state.lastPaused.IsZero() || now.Sub(state.lastPaused) >= p.cfg.PauseCooldown
	// pendingPause dedups concurrent Record callers so we never
	// fan out parallel PauseSource attempts. lastPaused stays
	// zero until the pause+audit pair fully commits.
	shouldPause := breach && cooled && !state.pendingPause
	if shouldPause {
		state.pendingPause = true
	}
	p.mu.Unlock()

	if !shouldPause {
		return nil
	}
	if err := p.pauser.PauseSource(ctx, tenantID, sourceID); err != nil {
		p.mu.Lock()
		state.pendingPause = false
		p.mu.Unlock()

		return err
	}
	trigger := "error_rate"
	if !rateBreach && consecBreach {
		trigger = "consecutive_failures"
	}
	meta := audit.JSONMap{
		"window_duration_seconds":       int64(p.cfg.WindowDuration / time.Second),
		"window_buckets":                p.cfg.WindowBuckets,
		"error_rate_threshold":          p.cfg.ErrorRateThreshold,
		"observed_total":                total,
		"observed_errors":               errs,
		"consecutive_failure_threshold": p.cfg.ConsecutiveFailureThreshold,
		"consecutive_failures":          consecutiveFailures,
		"trigger":                       trigger,
	}
	log := audit.NewAuditLog(tenantID, "system", audit.ActionSourceAutoPaused, "source", sourceID, meta, "")
	if err := p.writer.Create(ctx, log); err != nil {
		p.mu.Lock()
		state.pendingPause = false
		p.mu.Unlock()

		return err
	}
	p.mu.Lock()
	state.lastPaused = p.cfg.Now()
	state.pendingPause = false
	p.mu.Unlock()

	return nil
}

// advanceLocked rolls the bucket cursor forward to `now`. Each
// bucket spans WindowDuration / WindowBuckets. Empty intermediate
// buckets are zeroed so a long pause doesn't leak stale counters.
func (p *SourceAutoPauser) advanceLocked(state *autoPauseState, now time.Time) {
	bucketDur := p.cfg.WindowDuration / time.Duration(p.cfg.WindowBuckets)
	if bucketDur <= 0 {
		return
	}
	elapsed := now.Sub(state.cursorTime)
	if elapsed < bucketDur {
		return
	}
	steps := int(elapsed / bucketDur)
	if steps >= p.cfg.WindowBuckets {
		for i := range state.buckets {
			state.buckets[i] = autoPauseBucket{}
		}
		state.cursor = 0
		state.cursorTime = now

		return
	}
	for i := 0; i < steps; i++ {
		state.cursor = (state.cursor + 1) % p.cfg.WindowBuckets
		state.buckets[state.cursor] = autoPauseBucket{}
	}
	state.cursorTime = state.cursorTime.Add(time.Duration(steps) * bucketDur)
}

// windowTotalsLocked sums the live buckets.
func (p *SourceAutoPauser) windowTotalsLocked(state *autoPauseState) (total, errors int64) {
	for _, b := range state.buckets {
		total += b.total
		errors += b.errors
	}

	return total, errors
}
