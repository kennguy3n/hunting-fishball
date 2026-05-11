// graceful_degradation.go — Round-11 Task 18.
//
// Wraps every GORM-backed admin store lookup the retrieval
// handler consumes on the hot path with a per-call timeout +
// panic recovery so a slow/unreachable Postgres degrades
// gracefully (caller sees defaults) instead of returning 500.
//
// The lookups in scope:
//
//   - LatencyBudgetLookup  — per-tenant request latency budget
//   - CacheTTLLookup       — per-tenant semantic-cache TTL
//   - PinLookup            — per-(tenant, query) pinned hits
//
// QueryAnalyticsRecorder is already fail-open: the recorder
// catches the error and logs internally.
//
// On failure (timeout fires or the function panics) we:
//
//  1. Increment context_engine_gorm_store_lookup_errors_total{store=...}.
//  2. Emit a structured slog.Warn with tenant_id, store, and the
//     concrete failure (timeout / panic).
//  3. Return the documented fallback so the handler's downstream
//     logic continues unchanged.
package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// gracefulLookupTimeout is the per-lookup deadline. 200ms is the
// task-specified target — short enough that a single slow query
// can't blow the request budget, long enough that a healthy
// Postgres always returns within it. The value is fixed (no env
// override) because the retrieval handler must keep its hot path
// predictable; if you need a different budget, fan-out via the
// LatencyBudgetLookup itself.
const gracefulLookupTimeout = 200 * time.Millisecond

// safeLatencyBudgetLookup invokes fn under a 200ms deadline.
// Returns (budget, true) on success, (0, false) on any failure
// (timeout, panic, or fn returning ok=false). The handler is
// already coded to treat ok=false as "no override", so the
// fallback path is preserved.
func safeLatencyBudgetLookup(ctx context.Context, fn LatencyBudgetLookup, tenantID string) (time.Duration, bool) {
	if fn == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, gracefulLookupTimeout)
	defer cancel()

	type result struct {
		dur time.Duration
		ok  bool
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				observability.ObserveGORMStoreLookupError("latency_budget")
				slog.Warn("retrieval: latency_budget lookup panic", "tenant_id", tenantID, "panic", fmt.Sprint(r))
				ch <- result{}
			}
		}()
		dur, ok := fn(ctx, tenantID)
		ch <- result{dur: dur, ok: ok}
	}()

	select {
	case r := <-ch:
		return r.dur, r.ok
	case <-ctx.Done():
		observability.ObserveGORMStoreLookupError("latency_budget")
		slog.Warn("retrieval: latency_budget lookup timed out", "tenant_id", tenantID, "timeout", gracefulLookupTimeout)
		return 0, false
	}
}

// safeCacheTTLLookup invokes fn under a 200ms deadline. Returns
// the fallback on any failure so the cache Set proceeds with the
// previous TTL.
func safeCacheTTLLookup(ctx context.Context, fn CacheTTLLookup, tenantID string, fallback time.Duration) time.Duration {
	if fn == nil {
		return fallback
	}
	ctx, cancel := context.WithTimeout(ctx, gracefulLookupTimeout)
	defer cancel()

	ch := make(chan time.Duration, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				observability.ObserveGORMStoreLookupError("cache_ttl")
				slog.Warn("retrieval: cache_ttl lookup panic", "tenant_id", tenantID, "panic", fmt.Sprint(r))
				ch <- fallback
			}
		}()
		ch <- fn(ctx, tenantID, fallback)
	}()

	select {
	case ttl := <-ch:
		return ttl
	case <-ctx.Done():
		observability.ObserveGORMStoreLookupError("cache_ttl")
		slog.Warn("retrieval: cache_ttl lookup timed out", "tenant_id", tenantID, "timeout", gracefulLookupTimeout)
		return fallback
	}
}

// safePinLookup invokes fn under a 200ms deadline. Returns a
// nil slice on any failure so the handler proceeds without
// pins.
func safePinLookup(ctx context.Context, fn PinLookup, tenantID, query string) []PinnedHit {
	if fn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gracefulLookupTimeout)
	defer cancel()

	ch := make(chan []PinnedHit, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				observability.ObserveGORMStoreLookupError("pin_lookup")
				slog.Warn("retrieval: pin_lookup panic", "tenant_id", tenantID, "panic", fmt.Sprint(r))
				ch <- nil
			}
		}()
		ch <- fn(ctx, tenantID, query)
	}()

	select {
	case pins := <-ch:
		return pins
	case <-ctx.Done():
		observability.ObserveGORMStoreLookupError("pin_lookup")
		slog.Warn("retrieval: pin_lookup timed out", "tenant_id", tenantID, "timeout", gracefulLookupTimeout)
		return nil
	}
}
