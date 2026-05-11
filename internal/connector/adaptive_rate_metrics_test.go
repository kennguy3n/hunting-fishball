// Round-12 Task 13 — adaptive rate-limiter observability metrics.
//
// Asserts that Throttled bumps context_engine_adaptive_rate_halved_total
// and sets context_engine_adaptive_rate_current, and that a
// subsequent Healthy call updates the gauge without bumping the
// halve counter.
package connector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

func TestAdaptiveRate_ThrottledIncrementsHalveCounterAndSetsGauge(t *testing.T) {
	observability.AdaptiveRateHalvedTotal.Reset()
	observability.AdaptiveRateCurrent.Reset()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	bucket := newFakeBucket()
	a, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{
		BaseRate: 100,
		Bucket:   bucket,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if err := a.Throttled(context.Background(), "github"); err != nil {
		t.Fatalf("throttled: %v", err)
	}
	if got := testutil.ToFloat64(observability.AdaptiveRateHalvedTotal.WithLabelValues("github")); got != 1 {
		t.Fatalf("halve counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(observability.AdaptiveRateCurrent.WithLabelValues("github")); got != 50 {
		t.Fatalf("current gauge = %v, want 50 (halved from 100)", got)
	}

	// Second throttle halves again: 50 -> 25.
	if err := a.Throttled(context.Background(), "github"); err != nil {
		t.Fatalf("throttled 2: %v", err)
	}
	if got := testutil.ToFloat64(observability.AdaptiveRateHalvedTotal.WithLabelValues("github")); got != 2 {
		t.Fatalf("halve counter after 2 events = %v, want 2", got)
	}
	if got := testutil.ToFloat64(observability.AdaptiveRateCurrent.WithLabelValues("github")); got != 25 {
		t.Fatalf("current gauge after 2 events = %v, want 25", got)
	}
}

func TestAdaptiveRate_HealthyUpdatesGaugeNotHalveCounter(t *testing.T) {
	observability.AdaptiveRateHalvedTotal.Reset()
	observability.AdaptiveRateCurrent.Reset()

	step := 0
	now := func() time.Time {
		t := time.Date(2026, 1, 1, 12, 0, step, 0, time.UTC)
		step += 2 // every Now() call moves the clock forward 2s,
		// well past the default 1s CooldownInterval.
		return t
	}
	bucket := newFakeBucket()
	a, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{
		BaseRate: 100,
		Bucket:   bucket,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Drop the rate so Healthy has something to recover.
	if err := a.Throttled(context.Background(), "slack"); err != nil {
		t.Fatalf("throttled: %v", err)
	}
	halveBefore := testutil.ToFloat64(observability.AdaptiveRateHalvedTotal.WithLabelValues("slack"))

	if err := a.Healthy(context.Background(), "slack"); err != nil {
		t.Fatalf("healthy: %v", err)
	}
	gauge := testutil.ToFloat64(observability.AdaptiveRateCurrent.WithLabelValues("slack"))
	if gauge <= 50 || gauge > 100 {
		t.Fatalf("gauge after Healthy = %v; expected 50<gauge<=100 (RecoveryStep applied)", gauge)
	}
	halveAfter := testutil.ToFloat64(observability.AdaptiveRateHalvedTotal.WithLabelValues("slack"))
	if halveAfter != halveBefore {
		t.Fatalf("Healthy must not bump halve counter; before=%v after=%v", halveBefore, halveAfter)
	}
}
