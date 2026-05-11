package observability_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// gaugeValue reads a single Gauge's current value via the testutil
// helper. Returns 0 when the gauge is unregistered.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	return testutil.ToFloat64(g)
}

// TestPoolSampler_SampleOnceUpdatesEachGauge verifies that one
// synchronous SampleOnce call writes the source readings onto each
// of the three pool-health gauges.
func TestPoolSampler_SampleOnceUpdatesEachGauge(t *testing.T) {
	observability.ResetForTest()
	t.Cleanup(observability.ResetForTest)

	sampler := observability.NewPoolSampler(observability.PoolSamplerConfig{
		Postgres: observability.PostgresStatsFunc(func() int { return 7 }),
		Redis:    observability.RedisStatsFunc(func() int { return 3 }),
		Qdrant:   observability.QdrantStatsFunc(func() int { return 32 }),
	})

	sampler.SampleOnce()

	if got := gaugeValue(t, observability.PostgresPoolOpenConnections); got != 7 {
		t.Errorf("postgres gauge = %v; want 7", got)
	}
	if got := gaugeValue(t, observability.RedisPoolActiveConnections); got != 3 {
		t.Errorf("redis gauge = %v; want 3", got)
	}
	if got := gaugeValue(t, observability.QdrantPoolIdleConnections); got != 32 {
		t.Errorf("qdrant gauge = %v; want 32", got)
	}
}

// TestPoolSampler_NilSourcesAreSkipped confirms that an unconfigured
// source leaves the matching gauge untouched.
func TestPoolSampler_NilSourcesAreSkipped(t *testing.T) {
	observability.ResetForTest()
	t.Cleanup(observability.ResetForTest)

	// Only Redis is configured; Postgres / Qdrant gauges stay at 0.
	sampler := observability.NewPoolSampler(observability.PoolSamplerConfig{
		Redis: observability.RedisStatsFunc(func() int { return 11 }),
	})
	sampler.SampleOnce()

	if got := gaugeValue(t, observability.PostgresPoolOpenConnections); got != 0 {
		t.Errorf("postgres gauge = %v; want 0", got)
	}
	if got := gaugeValue(t, observability.RedisPoolActiveConnections); got != 11 {
		t.Errorf("redis gauge = %v; want 11", got)
	}
	if got := gaugeValue(t, observability.QdrantPoolIdleConnections); got != 0 {
		t.Errorf("qdrant gauge = %v; want 0", got)
	}
}

// TestPoolSampler_StartRunsImmediateSampleThenExitsOnCancel
// asserts the goroutine emits one sample before its first tick
// (so dashboards aren't blank during the initial Interval) and
// terminates cleanly when ctx is cancelled.
func TestPoolSampler_StartRunsImmediateSampleThenExitsOnCancel(t *testing.T) {
	observability.ResetForTest()
	t.Cleanup(observability.ResetForTest)

	sampler := observability.NewPoolSampler(observability.PoolSamplerConfig{
		Postgres: observability.PostgresStatsFunc(func() int { return 5 }),
		// 1h interval — we should never actually tick during the test.
		Interval: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sampler.Start(ctx)
	}()

	// Give the goroutine a beat to publish the immediate sample.
	time.Sleep(20 * time.Millisecond)

	if got := gaugeValue(t, observability.PostgresPoolOpenConnections); got != 5 {
		t.Errorf("immediate sample = %v; want 5", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not exit within 1s of cancel")
	}
}

// TestPoolSampler_NilReceiverIsSafe — defensive guard so callers
// can hold a *PoolSampler that might be nil without panicking.
func TestPoolSampler_NilReceiverIsSafe(t *testing.T) {
	var s *observability.PoolSampler
	s.SampleOnce()
	s.Start(context.Background())
}

// TestPoolSampler_GaugesExposedOnMetricsEndpoint smoke-checks that
// the three Round-9 Task-17 gauges land on /metrics with the
// expected names. Operators rely on these names in their alerts
// / dashboards; renaming would silently break their queries.
func TestPoolSampler_GaugesExposedOnMetricsEndpoint(t *testing.T) {
	observability.ResetForTest()
	t.Cleanup(observability.ResetForTest)

	sampler := observability.NewPoolSampler(observability.PoolSamplerConfig{
		Postgres: observability.PostgresStatsFunc(func() int { return 1 }),
		Redis:    observability.RedisStatsFunc(func() int { return 2 }),
		Qdrant:   observability.QdrantStatsFunc(func() int { return 3 }),
	})
	sampler.SampleOnce()

	mfs, err := observability.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"context_engine_postgres_pool_open_connections": false,
		"context_engine_redis_pool_active_connections":  false,
		"context_engine_qdrant_pool_idle_connections":   false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %s not exposed (want it on /metrics)", name)
		}
	}
	// Belt-and-suspenders: also check the help string for the
	// Postgres gauge so a copy-paste rename in metrics.go is
	// caught.
	for _, mf := range mfs {
		if mf.GetName() == "context_engine_postgres_pool_open_connections" {
			if !strings.Contains(mf.GetHelp(), "Postgres") {
				t.Errorf("postgres gauge Help does not mention 'Postgres': %q", mf.GetHelp())
			}
		}
	}
}
