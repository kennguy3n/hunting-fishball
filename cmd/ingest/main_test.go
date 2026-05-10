package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWaitChanClosed_DrainedChannelDoesNotBlock is the regression test
// for the pipeline-coordinator graceful-shutdown bug surfaced by Devin
// Review on PR #12. Pre-fix, the lifecycle step waited inline on
// `<-coordDone`; if the outer select had already consumed the value
// (coord.Run returned nil before SIGTERM), the second receive blocked
// for the full 30 s lifecycle deadline, starving every shutdown step
// that ran after it.
//
// The fix closes the channel after send. A receive on a closed channel
// returns the zero value immediately, so waitChanClosed must return nil
// well within the test's tight deadline rather than waiting for ctx
// expiry.
func TestWaitChanClosed_DrainedChannelDoesNotBlock(t *testing.T) {
	t.Parallel()

	ch := make(chan error, 1)
	ch <- nil
	close(ch)
	// Drain the value so we exercise the post-consumption path.
	<-ch

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := waitChanClosed(ctx, ch); err != nil {
		t.Fatalf("waitChanClosed: unexpected error %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("waitChanClosed blocked %s on a drained-and-closed channel", d)
	}
}

func TestWaitChanClosed_PendingValueReturnsImmediately(t *testing.T) {
	t.Parallel()

	ch := make(chan error, 1)
	ch <- nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitChanClosed(ctx, ch); err != nil {
		t.Fatalf("waitChanClosed: unexpected error %v", err)
	}
}

func TestWaitChanClosed_ProducerCloseAfterSendIsObservable(t *testing.T) {
	t.Parallel()

	ch := make(chan error, 1)
	go func() {
		ch <- nil
		close(ch)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitChanClosed(ctx, ch); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	// Second wait: simulates the lifecycle step running after the main
	// loop already consumed the value. With the closed-channel fix it
	// returns immediately; without it (the original bug) it hangs.
	start := time.Now()
	if err := waitChanClosed(ctx, ch); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("second wait blocked %s; close-after-send was lost", d)
	}
}

func TestWaitChanClosed_ContextDeadlineHonoured(t *testing.T) {
	t.Parallel()

	ch := make(chan error)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := waitChanClosed(ctx, ch)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

// TestStageWorkerEnv covers the env-var → int conversion that drives
// per-stage worker pool sizing in the coordinator. Empty / invalid
// values must round-trip to 0 so pipeline.StageConfig.defaults()
// applies its per-stage default of 1 and preserves the original
// single-goroutine semantics.
func TestStageWorkerEnv(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want int
	}{
		{"unset returns zero", "", 0},
		{"plain integer", "4", 4},
		{"single digit", "1", 1},
		{"zero is treated as default", "0", 0},
		{"negative is rejected", "-1", 0},
		{"non-numeric is rejected", "many", 0},
		{"trailing junk is rejected", "4x", 0},
		{"large value accepted", "128", 128},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STAGE_WORKER_TEST", tc.val)
			if tc.val == "" {
				if got := stageWorkerEnv("STAGE_WORKER_UNSET"); got != tc.want {
					t.Fatalf("got %d want %d", got, tc.want)
				}
				return
			}
			if got := stageWorkerEnv("STAGE_WORKER_TEST"); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
