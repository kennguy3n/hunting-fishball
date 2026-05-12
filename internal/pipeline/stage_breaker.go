// stage_breaker.go — Round-13 Task 5.
//
// Per-stage circuit breakers for the ingest pipeline coordinator.
// The gRPC sidecar pool already has connection-level breakers
// (internal/grpcpool/), but those only protect a single endpoint.
// A broken Parse or Embed stage can still burn the entire retry
// budget across N attempts per event because every attempt
// re-enters the sidecar and hits the (still-open) gRPC breaker.
//
// StageCircuitBreaker tracks consecutive failures across the
// stage's runWithRetry calls. When the failure count crosses the
// threshold the breaker opens; while open, new events from that
// stage are short-circuited to the DLQ instead of paying any
// retry budget. After the configured cooldown elapses the
// breaker half-opens and lets one probe through.
//
// Gated on CONTEXT_ENGINE_STAGE_BREAKER_ENABLED so existing
// deployments don't gain the new behaviour silently.
package pipeline

import (
	"errors"
	"sync"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// StageBreakerState enumerates the three breaker states.
type StageBreakerState int

const (
	// StageBreakerClosed is the healthy state — calls flow.
	StageBreakerClosed StageBreakerState = iota
	// StageBreakerOpen is the tripped state — calls
	// short-circuit immediately.
	StageBreakerOpen
	// StageBreakerHalfOpen is the recovery state — exactly
	// one probe call is allowed; success closes the breaker,
	// failure re-opens it.
	StageBreakerHalfOpen
)

func (s StageBreakerState) String() string {
	switch s {
	case StageBreakerOpen:
		return "open"
	case StageBreakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// ErrStageBreakerOpen is returned by Allow when the stage breaker
// is open and the caller should short-circuit to DLQ.
var ErrStageBreakerOpen = errors.New("stage breaker open")

// StageCircuitBreakerConfig configures one breaker instance.
type StageCircuitBreakerConfig struct {
	// Stage names the pipeline stage. One of the bounded set
	// {fetch, parse, embed, store}.
	Stage string
	// Threshold is the number of consecutive failures that
	// trip the breaker.
	Threshold int
	// OpenFor is how long the breaker stays open before
	// transitioning to half-open. Defaults to 30s.
	OpenFor time.Duration
	// NowFn allows tests to drive time deterministically.
	NowFn func() time.Time
}

// StageCircuitBreaker is the breaker instance. It is safe for
// concurrent use; the coordinator's stage goroutines hit a single
// breaker per stage.
type StageCircuitBreaker struct {
	cfg           StageCircuitBreakerConfig
	mu            sync.Mutex
	state         StageBreakerState
	fails         int
	openAt        time.Time
	probeInFlight bool
}

// NewStageCircuitBreaker validates and constructs the breaker.
func NewStageCircuitBreaker(cfg StageCircuitBreakerConfig) (*StageCircuitBreaker, error) {
	if cfg.Stage == "" {
		return nil, errors.New("stage breaker: Stage required")
	}
	if cfg.Threshold <= 0 {
		return nil, errors.New("stage breaker: Threshold must be positive")
	}
	if cfg.OpenFor <= 0 {
		cfg.OpenFor = 30 * time.Second
	}
	if cfg.NowFn == nil {
		cfg.NowFn = func() time.Time { return time.Now().UTC() }
	}
	return &StageCircuitBreaker{cfg: cfg, state: StageBreakerClosed}, nil
}

// State returns the current breaker state (test-friendly).
func (b *StageCircuitBreaker) State() StageBreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// StageBreakerSnapshot is the immutable observation shape returned
// by Snapshot. Round-14 Task 1 — the admin dashboard endpoint
// consumes a slice of these to render the per-stage state table.
//
// We pin a struct rather than expose the breaker's mutex-protected
// internals so the snapshot can be marshalled, logged, or cached
// without coupling to the breaker's locking discipline.
type StageBreakerSnapshot struct {
	Stage         string    `json:"stage"`
	State         string    `json:"state"`
	FailCount     int       `json:"fail_count"`
	OpenedAt      time.Time `json:"opened_at,omitempty"`
	ProbeInFlight bool      `json:"probe_in_flight"`
	Threshold     int       `json:"threshold"`
	OpenFor       string    `json:"open_for"`
}

// Snapshot returns a point-in-time view of the breaker state.
// Safe for concurrent use; the snapshot is copied under the
// breaker's mutex so callers never observe a half-updated row.
func (b *StageCircuitBreaker) Snapshot() StageBreakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	snap := StageBreakerSnapshot{
		Stage:         b.cfg.Stage,
		State:         b.state.String(),
		FailCount:     b.fails,
		ProbeInFlight: b.probeInFlight,
		Threshold:     b.cfg.Threshold,
		OpenFor:       b.cfg.OpenFor.String(),
	}
	if !b.openAt.IsZero() {
		snap.OpenedAt = b.openAt
	}
	return snap
}

// StageBreakerInspector is the narrow read seam an admin handler
// uses to render every registered breaker. cmd/api / cmd/ingest
// wire a *StageBreakerRegistry; tests pass an in-memory slice.
type StageBreakerInspector interface {
	Snapshot() []StageBreakerSnapshot
}

// StageBreakerRegistry holds a process-wide set of breakers keyed
// by stage. It is the production implementation of
// StageBreakerInspector. The registry is intentionally simple —
// breakers can only be added (the pipeline never tears them down
// at runtime) — so the read side does not need to guard against
// removals racing with iteration.
type StageBreakerRegistry struct {
	mu       sync.RWMutex
	breakers []*StageCircuitBreaker
}

// NewStageBreakerRegistry returns an empty registry.
func NewStageBreakerRegistry() *StageBreakerRegistry {
	return &StageBreakerRegistry{}
}

// Add registers a breaker with the registry. The breaker pointer
// itself is held — the registry observes live state on every
// Snapshot() call.
func (r *StageBreakerRegistry) Add(b *StageCircuitBreaker) {
	if r == nil || b == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.breakers = append(r.breakers, b)
}

// Snapshot returns one row per registered breaker in registration
// order. The slice is freshly allocated so callers can sort or
// mutate without affecting the registry.
func (r *StageBreakerRegistry) Snapshot() []StageBreakerSnapshot {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]StageBreakerSnapshot, 0, len(r.breakers))
	for _, b := range r.breakers {
		out = append(out, b.Snapshot())
	}
	return out
}

// Allow gates a stage call. It returns ErrStageBreakerOpen when
// the caller must short-circuit. The caller MUST report the
// outcome via OnSuccess or OnFailure so the breaker can update
// its state.
//
// In the half-open state exactly one probe is permitted at a
// time. Concurrent callers that arrive while a probe is in
// flight short-circuit so the stage isn't slammed by a thundering
// herd before the breaker has decided whether to close or re-open.
func (b *StageCircuitBreaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StageBreakerOpen {
		if b.cfg.NowFn().Sub(b.openAt) >= b.cfg.OpenFor {
			b.state = StageBreakerHalfOpen
			b.probeInFlight = true
			observability.StageBreakerStatesTotal.WithLabelValues(b.cfg.Stage, "half-open").Inc()
			return nil
		}
		observability.StageBreakerShortCircuitsTotal.WithLabelValues(b.cfg.Stage).Inc()
		return ErrStageBreakerOpen
	}
	if b.state == StageBreakerHalfOpen && b.probeInFlight {
		observability.StageBreakerShortCircuitsTotal.WithLabelValues(b.cfg.Stage).Inc()
		return ErrStageBreakerOpen
	}
	return nil
}

// OnSuccess marks the last call as healthy. Half-open → closed.
func (b *StageCircuitBreaker) OnSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
	b.probeInFlight = false
	if b.state != StageBreakerClosed {
		b.state = StageBreakerClosed
		observability.StageBreakerStatesTotal.WithLabelValues(b.cfg.Stage, "closed").Inc()
	}
}

// OnFailure marks the last call as failed. Closed → open after
// threshold failures. Half-open → open immediately on first
// failure (probe failed).
func (b *StageCircuitBreaker) OnFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StageBreakerHalfOpen {
		b.state = StageBreakerOpen
		b.openAt = b.cfg.NowFn()
		b.probeInFlight = false
		observability.StageBreakerStatesTotal.WithLabelValues(b.cfg.Stage, "open").Inc()
		return
	}
	b.fails++
	if b.fails >= b.cfg.Threshold && b.state == StageBreakerClosed {
		b.state = StageBreakerOpen
		b.openAt = b.cfg.NowFn()
		observability.StageBreakerStatesTotal.WithLabelValues(b.cfg.Stage, "open").Inc()
	}
}
