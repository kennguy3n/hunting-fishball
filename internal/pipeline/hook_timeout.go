package pipeline

// hook_timeout.go — Round-11 Task 5.
//
// Stage-4 coordinator hooks (chunk_quality, sync_history) write
// through GORM-backed stores on the chunk ingestion hot path. A
// slow Postgres write (long-running migration, replication lag,
// pgbouncer hiccup) MUST NOT block the pipeline — losing a quality
// score or sync-history row is acceptable, but losing the chunk
// itself is not.
//
// Every hook call from the coordinator is wrapped in
// hookCallTimeout, which budgets the underlying store call. When
// the deadline fires the wrapper increments
// observability.HookTimeoutsTotal{hook="..."} and returns the
// context.DeadlineExceeded error so the caller can log it and
// continue.
//
// The budget defaults to 500 ms and is configurable via the
// CONTEXT_ENGINE_HOOK_TIMEOUT env var (Go duration syntax, e.g.
// "250ms", "1s").

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// defaultHookTimeout is the budget applied when the env var is
// unset or unparseable.
const defaultHookTimeout = 500 * time.Millisecond

var (
	hookTimeoutOnce  sync.Once
	hookTimeoutValue time.Duration
)

// HookTimeout returns the configured per-hook deadline. Reads
// CONTEXT_ENGINE_HOOK_TIMEOUT once at first call; subsequent calls
// return the cached value. Invalid / negative values fall back to
// defaultHookTimeout.
func HookTimeout() time.Duration {
	hookTimeoutOnce.Do(func() {
		hookTimeoutValue = defaultHookTimeout
		raw := os.Getenv("CONTEXT_ENGINE_HOOK_TIMEOUT")
		if raw == "" {
			return
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return
		}
		hookTimeoutValue = d
	})
	return hookTimeoutValue
}

// ResetHookTimeoutForTest clears the cached value so a test can
// install a different env var without leaking state across runs.
func ResetHookTimeoutForTest() {
	hookTimeoutOnce = sync.Once{}
	hookTimeoutValue = 0
}

// runWithHookTimeout invokes fn under a derived context whose
// deadline is the lesser of the caller's deadline and the
// configured hook timeout. When fn does not return by the
// deadline, the wrapper records an observability counter increment
// with the hook label and returns context.DeadlineExceeded. The fn
// goroutine is allowed to continue in the background — the caller
// trades a goroutine for not blocking the pipeline. The store call
// itself will observe the cancelled context and exit promptly.
//
// hook is a bounded enumeration of label values (see
// observability.HookTimeoutsTotal); pass the matching string from
// the call site.
func runWithHookTimeout(ctx context.Context, hook string, fn func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(ctx, HookTimeout())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- fn(cctx)
	}()

	select {
	case err := <-done:
		return err
	case <-cctx.Done():
		observability.ObserveHookTimeout(hook)
		return cctx.Err()
	}
}
