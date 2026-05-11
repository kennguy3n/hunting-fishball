package pipeline_test

import (
	"errors"
	"strings"
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
