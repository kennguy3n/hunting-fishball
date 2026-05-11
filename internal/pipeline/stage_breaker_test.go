package pipeline_test

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestStageBreaker_RequiresStage(t *testing.T) {
	t.Parallel()
	_, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{Threshold: 3})
	if err == nil || !strings.Contains(err.Error(), "Stage") {
		t.Fatalf("err=%v", err)
	}
}

func TestStageBreaker_RequiresThreshold(t *testing.T) {
	t.Parallel()
	_, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{Stage: "embed"})
	if err == nil || !strings.Contains(err.Error(), "Threshold") {
		t.Fatalf("err=%v", err)
	}
}

func TestStageBreaker_TripsAfterThreshold(t *testing.T) {
	t.Parallel()
	b, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{Stage: "embed", Threshold: 3, OpenFor: time.Minute})
	if err != nil {
		t.Fatalf("NewStageCircuitBreaker: %v", err)
	}
	// Two failures still allow calls through.
	b.OnFailure()
	b.OnFailure()
	if err := b.Allow(); err != nil {
		t.Fatalf("Allow after 2 failures = %v", err)
	}
	if b.State() != pipeline.StageBreakerClosed {
		t.Fatalf("state=%s", b.State())
	}
	// Third failure trips the breaker.
	b.OnFailure()
	if b.State() != pipeline.StageBreakerOpen {
		t.Fatalf("state=%s after threshold", b.State())
	}
	if err := b.Allow(); !errors.Is(err, pipeline.ErrStageBreakerOpen) {
		t.Fatalf("Allow when open = %v", err)
	}
}

func TestStageBreaker_HalfOpenAfterCooldown(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	b, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{
		Stage:     "parse",
		Threshold: 1,
		OpenFor:   100 * time.Millisecond,
		NowFn:     func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewStageCircuitBreaker: %v", err)
	}
	b.OnFailure()
	if b.State() != pipeline.StageBreakerOpen {
		t.Fatalf("expected open")
	}
	// Still inside cooldown: short-circuits.
	if err := b.Allow(); !errors.Is(err, pipeline.ErrStageBreakerOpen) {
		t.Fatalf("inside cooldown: %v", err)
	}
	// Advance past cooldown.
	clock = clock.Add(200 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	if b.State() != pipeline.StageBreakerHalfOpen {
		t.Fatalf("expected half-open, got %s", b.State())
	}
	// Probe success → closed.
	b.OnSuccess()
	if b.State() != pipeline.StageBreakerClosed {
		t.Fatalf("after success: %s", b.State())
	}
}

func TestStageBreaker_HalfOpenFailureReopens(t *testing.T) {
	t.Parallel()
	clock := time.Now().UTC()
	b, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{
		Stage:     "embed",
		Threshold: 1,
		OpenFor:   100 * time.Millisecond,
		NowFn:     func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewStageCircuitBreaker: %v", err)
	}
	b.OnFailure()
	clock = clock.Add(200 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	// Probe fails → re-open.
	b.OnFailure()
	if b.State() != pipeline.StageBreakerOpen {
		t.Fatalf("expected re-open, got %s", b.State())
	}
}

// TestStageBreaker_HalfOpenAllowsSingleProbe verifies the
// invariant that exactly one Allow() call returns nil while the
// breaker is half-open. Concurrent callers must short-circuit so
// the failing stage isn't slammed by a thundering herd before the
// probe outcome is recorded.
func TestStageBreaker_HalfOpenAllowsSingleProbe(t *testing.T) {
	t.Parallel()
	clock := time.Now().UTC()
	b, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{
		Stage:     "embed",
		Threshold: 1,
		OpenFor:   100 * time.Millisecond,
		NowFn:     func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewStageCircuitBreaker: %v", err)
	}
	b.OnFailure()
	if b.State() != pipeline.StageBreakerOpen {
		t.Fatalf("expected open after first failure")
	}
	clock = clock.Add(200 * time.Millisecond)

	const concurrency = 64
	var (
		allowed      int64
		shortCircs   int64
		startBarrier sync.WaitGroup
		done         sync.WaitGroup
	)
	startBarrier.Add(1)
	done.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer done.Done()
			startBarrier.Wait()
			if err := b.Allow(); err == nil {
				atomic.AddInt64(&allowed, 1)
			} else if errors.Is(err, pipeline.ErrStageBreakerOpen) {
				atomic.AddInt64(&shortCircs, 1)
			} else {
				t.Errorf("unexpected error from Allow: %v", err)
			}
		}()
	}
	startBarrier.Done()
	done.Wait()

	if got := atomic.LoadInt64(&allowed); got != 1 {
		t.Fatalf("expected exactly 1 Allow to succeed during half-open probe, got %d", got)
	}
	if got := atomic.LoadInt64(&shortCircs); got != concurrency-1 {
		t.Fatalf("expected %d short-circuits, got %d", concurrency-1, got)
	}
	if b.State() != pipeline.StageBreakerHalfOpen {
		t.Fatalf("expected breaker to remain half-open until probe completes, got %s", b.State())
	}

	// Probe succeeds → breaker closes and subsequent callers
	// flow normally again.
	b.OnSuccess()
	if b.State() != pipeline.StageBreakerClosed {
		t.Fatalf("expected closed after probe success, got %s", b.State())
	}
	if err := b.Allow(); err != nil {
		t.Fatalf("post-close Allow: %v", err)
	}
}

// TestStageBreaker_HalfOpenProbeFailureReleasesGate verifies that
// a failed probe transitions back to Open AND clears the
// probe-in-flight gate so a subsequent half-open transition (after
// the next cooldown) can issue a fresh probe.
func TestStageBreaker_HalfOpenProbeFailureReleasesGate(t *testing.T) {
	t.Parallel()
	clock := time.Now().UTC()
	b, err := pipeline.NewStageCircuitBreaker(pipeline.StageCircuitBreakerConfig{
		Stage:     "parse",
		Threshold: 1,
		OpenFor:   100 * time.Millisecond,
		NowFn:     func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewStageCircuitBreaker: %v", err)
	}
	// Trip → cooldown → half-open probe fails → re-open.
	b.OnFailure()
	clock = clock.Add(200 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("first probe Allow: %v", err)
	}
	b.OnFailure()
	if b.State() != pipeline.StageBreakerOpen {
		t.Fatalf("expected re-open, got %s", b.State())
	}
	// Next cooldown should grant a fresh probe.
	clock = clock.Add(200 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("second probe Allow: %v", err)
	}
	if b.State() != pipeline.StageBreakerHalfOpen {
		t.Fatalf("expected half-open for second probe, got %s", b.State())
	}
}
