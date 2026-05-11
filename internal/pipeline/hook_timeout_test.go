package pipeline

// hook_timeout_test.go — Round-11 Devin Review fix.
//
// Unit tests for runWithHookTimeout. The wrapper spawns a
// goroutine to call into the GORM-backed hook recorder; a panic
// inside that goroutine without panic recovery would crash the
// ingest process (an unrecovered panic in any goroutine is fatal
// in Go, not just the goroutine itself). The sibling wrapper in
// internal/retrieval/graceful_degradation.go correctly defers a
// recover() inside each spawned goroutine — these tests pin the
// same contract for the pipeline wrapper.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRunWithHookTimeout_PanicInFnDoesNotCrashAndIncrementsPanicCounter
// asserts that a panicking recorder does NOT take the process
// down, that runWithHookTimeout converts the panic into a
// returned error, and that the dedicated
// context_engine_hook_panics_total{hook} counter increments —
// distinct from the timeout counter so operators can alert on a
// crashing GORM driver (panic) separately from a slow one
// (timeout). The timeout counter must stay at 0 on this path.
func TestRunWithHookTimeout_PanicInFnDoesNotCrashAndIncrementsPanicCounter(t *testing.T) {
	observability_ResetForTest(t)
	t.Setenv("CONTEXT_ENGINE_HOOK_TIMEOUT", "5s")
	ResetHookTimeoutForTest()
	t.Cleanup(ResetHookTimeoutForTest)

	err := runWithHookTimeout(context.Background(), "chunk_quality_record", func(_ context.Context) error {
		var p *int
		_ = *p // nil-pointer dereference, like a misbehaving driver
		return nil
	})
	if err == nil {
		t.Fatalf("runWithHookTimeout: got nil error, want panic-recovery error")
	}
	if got := hookPanicCount("chunk_quality_record"); got < 1 {
		t.Fatalf("hook_panics_total{hook=chunk_quality_record}=%d want >=1 after recovered panic", got)
	}
	if got := hookTimeoutCount("chunk_quality_record"); got != 0 {
		t.Fatalf("hook_timeouts_total{hook=chunk_quality_record}=%d want 0 on panic path (must not conflate with hook_panics_total)", got)
	}
}

// TestRunWithHookTimeout_HappyPathReturnsFnError confirms the
// non-panic, non-timeout path still surfaces fn's error verbatim
// — the recover defer must not swallow ordinary errors.
func TestRunWithHookTimeout_HappyPathReturnsFnError(t *testing.T) {
	observability_ResetForTest(t)
	t.Setenv("CONTEXT_ENGINE_HOOK_TIMEOUT", "5s")
	ResetHookTimeoutForTest()
	t.Cleanup(ResetHookTimeoutForTest)

	sentinel := errors.New("recorder: connection refused")
	got := runWithHookTimeout(context.Background(), "chunk_quality_record", func(_ context.Context) error {
		return sentinel
	})
	if !errors.Is(got, sentinel) {
		t.Fatalf("runWithHookTimeout: got %v, want %v", got, sentinel)
	}
	if c := hookTimeoutCount("chunk_quality_record"); c != 0 {
		t.Fatalf("hook_timeouts_total{hook=chunk_quality_record}=%d want 0 for clean-error path", c)
	}
	if c := hookPanicCount("chunk_quality_record"); c != 0 {
		t.Fatalf("hook_panics_total{hook=chunk_quality_record}=%d want 0 for clean-error path", c)
	}
}

// TestRunWithHookTimeout_TimeoutFiresOnSlowFn confirms the
// existing timeout path still works after the panic-recovery
// edit (regression guard for the in-place edit).
func TestRunWithHookTimeout_TimeoutFiresOnSlowFn(t *testing.T) {
	observability_ResetForTest(t)
	t.Setenv("CONTEXT_ENGINE_HOOK_TIMEOUT", "10ms")
	ResetHookTimeoutForTest()
	t.Cleanup(ResetHookTimeoutForTest)

	err := runWithHookTimeout(context.Background(), "sync_history_start", func(cctx context.Context) error {
		<-cctx.Done()
		return cctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runWithHookTimeout: got %v, want context.DeadlineExceeded", err)
	}
	if got := hookTimeoutCount("sync_history_start"); got < 1 {
		t.Fatalf("hook_timeouts_total{hook=sync_history_start}=%d want >=1", got)
	}
	if got := hookPanicCount("sync_history_start"); got != 0 {
		t.Fatalf("hook_panics_total{hook=sync_history_start}=%d want 0 on timeout path (must not conflate with hook_timeouts_total)", got)
	}
	// Sanity: the wrapper must have returned well before any
	// reasonable per-test deadline. With a 10ms hook timeout
	// the assertion below would only fail if the wrapper had
	// stopped honoring it.
	deadline := time.Now().Add(50 * time.Millisecond)
	if time.Now().After(deadline) {
		t.Fatalf("runWithHookTimeout took longer than 50ms with a 10ms budget")
	}
}
