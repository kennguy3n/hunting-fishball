package retrieval

// graceful_degradation_test.go — Round-11 Task 18.
//
// Unit-tests the safe lookup wrappers in graceful_degradation.go
// in isolation. The handler-level test in round8_handler_test.go
// already covers the happy path through gin; this file exercises
// the timeout + panic fallback branches that are awkward to
// trigger end-to-end.

import (
	"context"
	"testing"
	"time"
)

// TestSafeLatencyBudgetLookup_TimesOut asserts the wrapper
// returns ok=false when the inner function blocks past the
// gracefulLookupTimeout.
func TestSafeLatencyBudgetLookup_TimesOut(t *testing.T) {
	t.Parallel()
	slow := LatencyBudgetLookup(func(ctx context.Context, _ string) (time.Duration, bool) {
		<-ctx.Done()
		return 0, false
	})
	start := time.Now()
	_, ok := safeLatencyBudgetLookup(context.Background(), slow, "t-1")
	elapsed := time.Since(start)
	if ok {
		t.Fatal("wrapper should return ok=false on timeout")
	}
	// 200ms is the documented timeout; allow generous slack so
	// the test stays stable on busy CI.
	if elapsed < gracefulLookupTimeout-50*time.Millisecond {
		t.Fatalf("wrapper returned too fast: %v", elapsed)
	}
	if elapsed > gracefulLookupTimeout+500*time.Millisecond {
		t.Fatalf("wrapper exceeded timeout budget: %v", elapsed)
	}
}

// TestSafeLatencyBudgetLookup_Panic asserts the wrapper recovers
// from a panicking lookup and returns the documented fallback.
func TestSafeLatencyBudgetLookup_Panic(t *testing.T) {
	t.Parallel()
	boom := LatencyBudgetLookup(func(context.Context, string) (time.Duration, bool) {
		panic("simulated postgres outage")
	})
	dur, ok := safeLatencyBudgetLookup(context.Background(), boom, "t-1")
	if ok {
		t.Fatal("wrapper should return ok=false on panic")
	}
	if dur != 0 {
		t.Fatalf("dur = %v, want 0", dur)
	}
}

// TestSafeLatencyBudgetLookup_Success asserts the wrapper passes
// the value through when the inner function returns promptly.
func TestSafeLatencyBudgetLookup_Success(t *testing.T) {
	t.Parallel()
	fn := LatencyBudgetLookup(func(context.Context, string) (time.Duration, bool) {
		return 80 * time.Millisecond, true
	})
	dur, ok := safeLatencyBudgetLookup(context.Background(), fn, "t-1")
	if !ok || dur != 80*time.Millisecond {
		t.Fatalf("got (%v, %v), want (80ms, true)", dur, ok)
	}
}

// TestSafeCacheTTLLookup_TimesOut_ReturnsFallback asserts the
// wrapper returns the supplied fallback on timeout.
func TestSafeCacheTTLLookup_TimesOut_ReturnsFallback(t *testing.T) {
	t.Parallel()
	slow := CacheTTLLookup(func(ctx context.Context, _ string, fallback time.Duration) time.Duration {
		<-ctx.Done()
		return fallback * 2 // would be wrong if returned
	})
	got := safeCacheTTLLookup(context.Background(), slow, "t-1", 5*time.Minute)
	if got != 5*time.Minute {
		t.Fatalf("got %v, want fallback 5m", got)
	}
}

// TestSafePinLookup_TimesOut_ReturnsNil asserts the wrapper
// returns nil on timeout so the handler skips pin splicing.
func TestSafePinLookup_TimesOut_ReturnsNil(t *testing.T) {
	t.Parallel()
	slow := PinLookup(func(ctx context.Context, _, _ string) []PinnedHit {
		<-ctx.Done()
		return []PinnedHit{{ChunkID: "should-not-appear", Position: 0}}
	})
	got := safePinLookup(context.Background(), slow, "t-1", "hello")
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
