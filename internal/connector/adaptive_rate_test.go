package connector

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeBucket struct {
	mu    sync.Mutex
	rates map[string]float64
}

func newFakeBucket() *fakeBucket { return &fakeBucket{rates: map[string]float64{}} }

func (f *fakeBucket) Wait(_ context.Context, _ string) error { return nil }

func (f *fakeBucket) SetRate(_ context.Context, key string, rate float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rates[key] = rate
	return nil
}

func (f *fakeBucket) Rate(key string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rates[key]
}

func TestNewAdaptiveRateLimiter_Validation(t *testing.T) {
	if _, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{}); err == nil {
		t.Fatalf("nil bucket + zero rate must error")
	}
	if _, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{BaseRate: 10}); err == nil {
		t.Fatalf("nil bucket must error")
	}
	if _, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{BaseRate: 10, Bucket: newFakeBucket()}); err != nil {
		t.Fatalf("happy path: %v", err)
	}
}

func TestAdaptiveRateLimiter_ThrottlesOnSignal(t *testing.T) {
	bucket := newFakeBucket()
	a, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{BaseRate: 100, FloorRate: 5, Bucket: bucket})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if a.CurrentRate() != 100 {
		t.Fatalf("initial rate must be base; got %v", a.CurrentRate())
	}
	if err := a.Throttled(context.Background(), "src-1"); err != nil {
		t.Fatalf("throttle: %v", err)
	}
	if got := a.CurrentRate(); got != 50 {
		t.Fatalf("first throttle should halve; got %v", got)
	}
	if got := bucket.Rate("src-1"); got != 50 {
		t.Fatalf("bucket not updated; got %v", got)
	}
	// Repeated 429s drop further but not below floor.
	for i := 0; i < 20; i++ {
		_ = a.Throttled(context.Background(), "src-1")
	}
	if got := a.CurrentRate(); got != 5 {
		t.Fatalf("rate must clamp at floor; got %v", got)
	}
}

func TestAdaptiveRateLimiter_GradualRecovery(t *testing.T) {
	bucket := newFakeBucket()
	now := time.Now()
	a, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{
		BaseRate:         100,
		FloorRate:        5,
		RecoveryStep:     10,
		CooldownInterval: 100 * time.Millisecond,
		Bucket:           bucket,
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	for i := 0; i < 10; i++ {
		_ = a.Throttled(context.Background(), "src-1")
	}
	if got := a.CurrentRate(); got != 5 {
		t.Fatalf("expected rate at floor; got %v", got)
	}
	// Healthy without cooldown elapsed → no bump.
	_ = a.Healthy(context.Background(), "src-1")
	if got := a.CurrentRate(); got != 5 {
		t.Fatalf("Healthy without cooldown must not bump; got %v", got)
	}
	// Advance time and call Healthy 12 times (each bumps by 10
	// from floor 5 → 15 → 25 → ... → 95 → 100 (clamped)).
	for i := 0; i < 20; i++ {
		now = now.Add(200 * time.Millisecond)
		_ = a.Healthy(context.Background(), "src-1")
	}
	if got := a.CurrentRate(); got != 100 {
		t.Fatalf("expected rate at base; got %v", got)
	}
}

func TestAdaptiveRateLimiter_FloorRespected(t *testing.T) {
	bucket := newFakeBucket()
	a, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{
		BaseRate:       100,
		FloorRate:      25,
		ThrottleFactor: 0.1, // aggressive
		Bucket:         bucket,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	for i := 0; i < 5; i++ {
		_ = a.Throttled(context.Background(), "src-1")
	}
	if got := a.CurrentRate(); got != 25 {
		t.Fatalf("floor not respected; got %v", got)
	}
}

func TestAdaptiveRateLimiter_Concurrency(t *testing.T) {
	bucket := newFakeBucket()
	a, err := NewAdaptiveRateLimiter(AdaptiveRateLimiterConfig{BaseRate: 100, FloorRate: 5, Bucket: bucket})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Throttled(context.Background(), "src-1")
		}()
	}
	wg.Wait()
	// Floor must be respected even under concurrent throttles.
	if got := a.CurrentRate(); got < a.FloorRate() {
		t.Fatalf("rate dropped below floor under concurrency; got %v", got)
	}
}
