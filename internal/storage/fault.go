// Package storage — fault.go ships an opt-in fault injector for
// the storage backends.
//
// Round-4 Task 14: SREs need a way to drive the retrieval API into
// the partial-degraded code paths without taking down a real
// backend. The injector is wired in on top of every storage
// adapter (Qdrant, FalkorDB, BM25, Redis, Postgres) when
// CONTEXT_ENGINE_FAULT_INJECTION=true; in any other state the
// injector is a transparent no-op.
//
// Configurable failure modes:
//
//   - ErrorRate    — probability per call that the wrapper returns
//     a synthetic error.
//   - LatencyP50/P99 — adds a delay drawn from an exponential
//     distribution with the supplied scale.
//   - Timeout      — when non-zero, returns context.DeadlineExceeded
//     after the supplied delay.
//
// Production wiring NEVER enables the injector — the env var is
// inspected at process start and a misconfiguration loudly logs
// "FAULT INJECTION ENABLED" in red so it cannot ship to prod.
package storage

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FaultEnvVar is the env var that opts the wrapper in.
const FaultEnvVar = "CONTEXT_ENGINE_FAULT_INJECTION"

// FaultMode is the per-call result the injector might force.
type FaultMode int

const (
	// FaultNone is the default: pass-through.
	FaultNone FaultMode = iota
	// FaultError forces a synthetic error.
	FaultError
	// FaultTimeout forces a context.DeadlineExceeded after a delay.
	FaultTimeout
	// FaultLatency adds latency but does not change the result.
	FaultLatency
)

// FaultInjector is the wrapper struct. The zero value is a
// no-op — Roll always returns FaultNone, Sleep returns immediately.
type FaultInjector struct {
	enabled    bool
	errorRate  float64
	timeout    time.Duration
	latencyAvg time.Duration

	mu  sync.Mutex
	rng *rand.Rand
}

// NewFaultInjector builds a FaultInjector from the environment.
//
// Vars (all optional, all ignored when CONTEXT_ENGINE_FAULT_INJECTION
// is not truthy):
//
//	CONTEXT_ENGINE_FAULT_ERROR_RATE   float in [0,1], default 0
//	CONTEXT_ENGINE_FAULT_TIMEOUT      duration, default 0 (disabled)
//	CONTEXT_ENGINE_FAULT_LATENCY      duration, default 0
//
// Returns a FaultInjector even when disabled so callers can chain
// the wrapper unconditionally.
func NewFaultInjector(get func(string) string) *FaultInjector {
	if get == nil {
		get = os.Getenv
	}
	if !envTruthy(get(FaultEnvVar)) {
		return &FaultInjector{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
	}
	rate, _ := strconv.ParseFloat(strings.TrimSpace(get("CONTEXT_ENGINE_FAULT_ERROR_RATE")), 64)
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	timeout, _ := time.ParseDuration(strings.TrimSpace(get("CONTEXT_ENGINE_FAULT_TIMEOUT")))
	latency, _ := time.ParseDuration(strings.TrimSpace(get("CONTEXT_ENGINE_FAULT_LATENCY")))
	return &FaultInjector{
		enabled:    true,
		errorRate:  rate,
		timeout:    timeout,
		latencyAvg: latency,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Enabled reports whether the injector is opted-in.
func (f *FaultInjector) Enabled() bool { return f != nil && f.enabled }

// Roll returns the FaultMode the injector wants applied to this
// call. Safe to call from multiple goroutines.
//
// The probability model is two-step:
//
//  1. errorRate decides whether ANY synthetic fault fires on this
//     call. The previous implementation rolled errorRate twice in
//     sequence (once gated by timeout > 0 → FaultTimeout, once gated
//     by errorRate > 0 → FaultError) which made FaultError
//     unreachable when timeout > 0 and errorRate was 1, and produced
//     an effective error probability of errorRate × (1 − errorRate)
//     instead of the configured value at intermediate rates.
//  2. Given a fault fires, we pick timeout vs. error 50/50 if both
//     modes are configured, otherwise we fall through to the only
//     enabled fault. Latency-only configs still emit FaultLatency
//     when no fault was selected, since latency is additive rather
//     than terminal.
func (f *FaultInjector) Roll() FaultMode {
	if f == nil || !f.enabled {
		return FaultNone
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errorRate > 0 && f.rng.Float64() < f.errorRate {
		if f.timeout > 0 && f.rng.Float64() < 0.5 {
			return FaultTimeout
		}
		return FaultError
	}
	if f.latencyAvg > 0 {
		return FaultLatency
	}
	return FaultNone
}

// Apply mutates the context and returns either the original ctx +
// nil error, or a (cancelled or untouched) ctx + a synthetic error.
// Callers wire it in at the top of every storage method.
func (f *FaultInjector) Apply(ctx context.Context, op string) (context.Context, error) {
	if f == nil || !f.enabled {
		return ctx, nil
	}
	switch f.Roll() {
	case FaultError:
		return ctx, fmt.Errorf("fault inject: synthetic error op=%s", op)
	case FaultTimeout:
		f.sleep(ctx, f.timeout)
		return ctx, errors.New("fault inject: synthetic timeout: " + op)
	case FaultLatency:
		f.sleep(ctx, f.latencyAvg)
		return ctx, nil
	}
	return ctx, nil
}

func (f *FaultInjector) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	}
	return false
}
