package connector

// adaptive_rate.go — Round-6 Task 7.
//
// AdaptiveRateLimiter wraps a fixed-rate token bucket and reduces
// the effective rate whenever the upstream returns HTTP 429 (or any
// caller-flagged rate-limit signal). The underlying bucket is
// consulted for token availability; the adaptive layer is purely
// rate adjustment.
//
// Behaviour:
//
//   - Initial rate = BaseRate (per-second).
//   - On Throttled(): rate halves down to FloorRate (default 10%
//     of BaseRate).
//   - On Healthy() (any successful operation): rate steps back up
//     by `RecoveryStep` per call until it returns to BaseRate.
//   - The adaptive layer never raises rate above BaseRate.
//
// Calls are concurrency-safe; the rate is held under a mutex.

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RateBucket is the narrow port the adaptive layer adapts. The
// production wiring fills this with a Redis-backed token bucket
// (admin.RateLimiter) — for tests we use the in-memory adapter
// below.
type RateBucket interface {
	// Wait blocks until at least one token is available for key, or
	// returns an error if ctx is cancelled.
	Wait(ctx context.Context, key string) error
	// SetRate updates the bucket's refill rate (tokens/sec) for
	// key. Adaptive uses this to throttle on 429.
	SetRate(ctx context.Context, key string, ratePerSecond float64) error
}

// AdaptiveRateLimiterConfig configures an AdaptiveRateLimiter.
type AdaptiveRateLimiterConfig struct {
	// BaseRate is the steady-state rate per second. Required.
	BaseRate float64
	// FloorRate is the minimum rate the limiter ever drops to.
	// Defaults to 10% of BaseRate (with a 0.1/sec floor).
	FloorRate float64
	// ThrottleFactor is the multiplicative reduction applied per
	// throttled signal. Defaults to 0.5 (halve on each 429).
	ThrottleFactor float64
	// RecoveryStep is the additive bump applied per healthy
	// signal. Defaults to BaseRate / 10.
	RecoveryStep float64
	// CooldownInterval is the minimum time between recovery
	// steps. Defaults to 1s.
	CooldownInterval time.Duration
	// Bucket is the underlying rate bucket. Required.
	Bucket RateBucket
	// Now is the time source (for tests). Defaults to time.Now.
	Now func() time.Time
}

// AdaptiveRateLimiter is an adaptive wrapper around a token bucket.
type AdaptiveRateLimiter struct {
	cfg     AdaptiveRateLimiterConfig
	mu      sync.Mutex
	current float64
	lastUp  time.Time
}

// NewAdaptiveRateLimiter validates cfg and returns the limiter.
func NewAdaptiveRateLimiter(cfg AdaptiveRateLimiterConfig) (*AdaptiveRateLimiter, error) {
	if cfg.BaseRate <= 0 {
		return nil, errors.New("adaptive rate: BaseRate must be positive")
	}
	if cfg.Bucket == nil {
		return nil, errors.New("adaptive rate: nil Bucket")
	}
	if cfg.FloorRate <= 0 {
		cfg.FloorRate = max64(cfg.BaseRate*0.1, 0.1)
	}
	if cfg.FloorRate > cfg.BaseRate {
		cfg.FloorRate = cfg.BaseRate
	}
	if cfg.ThrottleFactor <= 0 || cfg.ThrottleFactor >= 1 {
		cfg.ThrottleFactor = 0.5
	}
	if cfg.RecoveryStep <= 0 {
		cfg.RecoveryStep = cfg.BaseRate / 10
	}
	if cfg.CooldownInterval <= 0 {
		cfg.CooldownInterval = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &AdaptiveRateLimiter{cfg: cfg, current: cfg.BaseRate, lastUp: cfg.Now()}, nil
}

// Wait acquires a token under the current adaptive rate.
func (a *AdaptiveRateLimiter) Wait(ctx context.Context, key string) error {
	return a.cfg.Bucket.Wait(ctx, key)
}

// Throttled is called when the upstream signals rate-limit (e.g.
// HTTP 429). The rate is multiplicatively reduced down to
// FloorRate and the new rate is pushed to the underlying bucket.
func (a *AdaptiveRateLimiter) Throttled(ctx context.Context, key string) error {
	a.mu.Lock()
	a.current = a.current * a.cfg.ThrottleFactor
	if a.current < a.cfg.FloorRate {
		a.current = a.cfg.FloorRate
	}
	rate := a.current
	a.mu.Unlock()
	return a.cfg.Bucket.SetRate(ctx, key, rate)
}

// Healthy is called after a successful upstream operation. The
// rate steps back up by RecoveryStep, but no faster than once per
// CooldownInterval. The new rate is pushed to the underlying
// bucket.
func (a *AdaptiveRateLimiter) Healthy(ctx context.Context, key string) error {
	a.mu.Lock()
	now := a.cfg.Now()
	if now.Sub(a.lastUp) < a.cfg.CooldownInterval {
		a.mu.Unlock()
		return nil
	}
	if a.current >= a.cfg.BaseRate {
		a.lastUp = now
		a.mu.Unlock()
		return nil
	}
	a.current += a.cfg.RecoveryStep
	if a.current > a.cfg.BaseRate {
		a.current = a.cfg.BaseRate
	}
	rate := a.current
	a.lastUp = now
	a.mu.Unlock()
	return a.cfg.Bucket.SetRate(ctx, key, rate)
}

// CurrentRate returns the present adaptive rate per second. Used
// by metrics and tests.
func (a *AdaptiveRateLimiter) CurrentRate() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current
}

// FloorRate returns the configured floor.
func (a *AdaptiveRateLimiter) FloorRate() float64 { return a.cfg.FloorRate }

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
