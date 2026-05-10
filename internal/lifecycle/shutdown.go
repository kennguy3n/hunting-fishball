// Package lifecycle owns the ordered graceful-shutdown helpers shared
// by cmd/api and cmd/ingest (Phase 8 / Task 10).
//
// Production binaries register a sequence of shutdown steps — stop
// accepting new work, drain in-flight work, close storage connections
// — and the helper runs them in registration order while honoring a
// single deadline. Steps that exceed their slice of the deadline are
// logged but the helper continues so a stalled Redis client doesn't
// prevent the Postgres connection pool from closing.
package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// Step is one phase of graceful shutdown. The function receives a
// context whose deadline is the overall shutdown budget; well-behaved
// steps respect it.
type Step struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Shutdown executes Steps in order under a single context deadline.
// The first non-nil error is returned but every remaining step still
// runs so resources aren't leaked. context.Canceled / DeadlineExceeded
// from Step.Fn is logged but not returned because that's the expected
// outcome when an operator forces a tight budget.
type Shutdown struct {
	logger  *slog.Logger
	steps   []Step
	timeout time.Duration
	ran     atomic.Bool
}

// New returns a Shutdown with the supplied per-step deadline. Defaults
// to 30s if zero. Logger defaults to slog.Default().
func New(timeout time.Duration, logger *slog.Logger) *Shutdown {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Shutdown{logger: logger, timeout: timeout}
}

// Add appends a step. Calls after Run are ignored.
func (s *Shutdown) Add(name string, fn func(ctx context.Context) error) {
	if name == "" || fn == nil {
		return
	}
	s.steps = append(s.steps, Step{Name: name, Fn: fn})
}

// Run executes all registered steps under a single shared deadline.
// Returns the first non-nil non-context error.
func (s *Shutdown) Run(parent context.Context) error {
	if !s.ran.CompareAndSwap(false, true) {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()
	var firstErr error
	for _, step := range s.steps {
		started := time.Now()
		err := step.Fn(ctx)
		dur := time.Since(started)
		switch {
		case err == nil:
			s.logger.Info("lifecycle: shutdown step ok", slog.String("step", step.Name), slog.Duration("dur", dur))
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			s.logger.Warn("lifecycle: shutdown step deadline", slog.String("step", step.Name), slog.Duration("dur", dur))
		default:
			s.logger.Warn("lifecycle: shutdown step error", slog.String("step", step.Name), slog.String("error", err.Error()), slog.Duration("dur", dur))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Steps exposes the registered Steps for tests.
func (s *Shutdown) Steps() []Step { return s.steps }
