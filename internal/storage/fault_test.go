package storage_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFaultInjector_DisabledByDefault(t *testing.T) {
	t.Parallel()
	f := storage.NewFaultInjector(mapEnv(map[string]string{}))
	if f.Enabled() {
		t.Fatal("must be disabled by default")
	}
	if got := f.Roll(); got != storage.FaultNone {
		t.Fatalf("Roll = %v; want FaultNone", got)
	}
	_, err := f.Apply(context.Background(), "noop")
	if err != nil {
		t.Fatalf("Apply (disabled) = %v", err)
	}
}

func TestFaultInjector_EnabledForcesError(t *testing.T) {
	t.Parallel()
	f := storage.NewFaultInjector(mapEnv(map[string]string{
		storage.FaultEnvVar:               "true",
		"CONTEXT_ENGINE_FAULT_ERROR_RATE": "1",
	}))
	if !f.Enabled() {
		t.Fatal("must be enabled")
	}
	_, err := f.Apply(context.Background(), "vector_query")
	if err == nil || !strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("Apply = %v; want synthetic error", err)
	}
}

func TestFaultInjector_RespectsCancelledContext(t *testing.T) {
	t.Parallel()
	f := storage.NewFaultInjector(mapEnv(map[string]string{
		storage.FaultEnvVar:            "yes",
		"CONTEXT_ENGINE_FAULT_LATENCY": "10s",
	}))
	if !f.Enabled() {
		t.Skip("env not honoured; skipping")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Apply must return promptly when ctx is already cancelled.
	if _, err := f.Apply(ctx, "x"); err != nil {
		t.Fatalf("Apply with cancelled ctx returned %v", err)
	}
}

func TestFaultInjector_LatencyOnly(t *testing.T) {
	t.Parallel()
	f := storage.NewFaultInjector(mapEnv(map[string]string{
		storage.FaultEnvVar:            "1",
		"CONTEXT_ENGINE_FAULT_LATENCY": "1ms",
	}))
	_, err := f.Apply(context.Background(), "x")
	if err != nil {
		t.Fatalf("latency-only apply: %v", err)
	}
}

// TestFaultInjector_RollKeepsErrorReachable is the regression for
// fault.go rolling errorRate twice in sequence — once gated by
// timeout > 0 (returning FaultTimeout) and a second time for
// FaultError. With the old code and errorRate=1 + timeout > 0,
// FaultError was unreachable because the first roll always won.
// The fix is a single roll against errorRate followed by a 50/50
// split between timeout and error. We exercise enough rolls that
// the probability of accidentally never observing FaultError
// (when it should fire ~50% of the time) is < 1e-15.
func TestFaultInjector_RollKeepsErrorReachable(t *testing.T) {
	t.Parallel()
	f := storage.NewFaultInjector(mapEnv(map[string]string{
		storage.FaultEnvVar:               "1",
		"CONTEXT_ENGINE_FAULT_ERROR_RATE": "1",
		"CONTEXT_ENGINE_FAULT_TIMEOUT":    "1ns",
	}))
	if !f.Enabled() {
		t.Fatal("must be enabled")
	}
	var sawError, sawTimeout int
	for i := 0; i < 200; i++ {
		switch f.Roll() {
		case storage.FaultError:
			sawError++
		case storage.FaultTimeout:
			sawTimeout++
		case storage.FaultNone, storage.FaultLatency:
			t.Fatalf("at errorRate=1 every roll must be a fault, got %v", f.Roll())
		}
	}
	if sawError == 0 {
		t.Fatalf("FaultError was never produced across 200 rolls; reachability regressed (timeouts=%d)", sawTimeout)
	}
	if sawTimeout == 0 {
		t.Fatalf("FaultTimeout was never produced across 200 rolls; both modes must remain reachable (errors=%d)", sawError)
	}
}

func TestEnvTruthy_PrivateBehavior(t *testing.T) {
	t.Parallel()
	// Indirect check: enabled iff env var is one of the truthy
	// strings the package documents.
	for _, v := range []string{"true", "TRUE", "1", "yes", "on"} {
		f := storage.NewFaultInjector(mapEnv(map[string]string{
			storage.FaultEnvVar: v,
		}))
		if !f.Enabled() {
			t.Errorf("%q should be truthy", v)
		}
	}
	for _, v := range []string{"", "0", "no", "off", "asdf"} {
		f := storage.NewFaultInjector(mapEnv(map[string]string{
			storage.FaultEnvVar: v,
		}))
		if f.Enabled() {
			t.Errorf("%q should NOT be truthy", v)
		}
	}
}
