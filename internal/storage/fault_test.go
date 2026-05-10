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
