package lifecycle_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/lifecycle"
)

func TestShutdown_RunsStepsInOrder(t *testing.T) {
	t.Parallel()
	s := lifecycle.New(time.Second, nil)
	var order []string
	s.Add("a", func(_ context.Context) error { order = append(order, "a"); return nil })
	s.Add("b", func(_ context.Context) error { order = append(order, "b"); return nil })
	s.Add("c", func(_ context.Context) error { order = append(order, "c"); return nil })
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("order=%v", order)
	}
}

func TestShutdown_FirstErrorReturned_StepsContinue(t *testing.T) {
	t.Parallel()
	s := lifecycle.New(time.Second, nil)
	wantErr := errors.New("boom")
	var thirdRan atomic.Bool
	s.Add("ok", func(_ context.Context) error { return nil })
	s.Add("err", func(_ context.Context) error { return wantErr })
	s.Add("late", func(_ context.Context) error { thirdRan.Store(true); return nil })
	err := s.Run(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("first error mismatch: %v", err)
	}
	if !thirdRan.Load() {
		t.Fatalf("late step should still run")
	}
}

func TestShutdown_DeadlineExceededIsNotReturned(t *testing.T) {
	t.Parallel()
	s := lifecycle.New(50*time.Millisecond, nil)
	s.Add("slow", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			return nil
		}
	})
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("deadline should be swallowed: %v", err)
	}
}

func TestShutdown_RunOnce(t *testing.T) {
	t.Parallel()
	s := lifecycle.New(time.Second, nil)
	calls := 0
	s.Add("inc", func(_ context.Context) error { calls++; return nil })
	_ = s.Run(context.Background())
	_ = s.Run(context.Background())
	if calls != 1 {
		t.Fatalf("expected 1, got %d", calls)
	}
}

func TestShutdown_AddNoopOnEmpty(t *testing.T) {
	t.Parallel()
	s := lifecycle.New(0, nil)
	s.Add("", func(_ context.Context) error { return nil })
	s.Add("ok", nil)
	if got := len(s.Steps()); got != 0 {
		t.Fatalf("expected zero steps, got %d", got)
	}
}
