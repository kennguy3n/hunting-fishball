package admin_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func newLimiter(t *testing.T, lim admin.RateLimit) (*admin.RateLimiter, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	rl, err := admin.NewRateLimiter(context.Background(), admin.RateLimiterConfig{
		Client: rc, KeyPrefix: "hf:rl", Limit: lim,
	})
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	return rl, mr, rc
}

func TestRateLimiter_AllowsBurstUpToCapacity(t *testing.T) {
	t.Parallel()
	rl, _, _ := newLimiter(t, admin.RateLimit{Capacity: 5, RefillPerSecond: 1})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		ok, retry, err := rl.Allow(ctx, "tenant-a", "src-1")
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		if !ok {
			t.Fatalf("burst should admit %d, got rejection (retry=%v)", i, retry)
		}
	}
	ok, retry, err := rl.Allow(ctx, "tenant-a", "src-1")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if ok {
		t.Fatalf("6th request must be throttled")
	}
	if retry <= 0 {
		t.Fatal("retry must be positive when throttled")
	}
}

func TestRateLimiter_Refills(t *testing.T) {
	t.Parallel()
	// Refill window is intentionally wide (250 ms / token) so neither
	// inter-call scheduling jitter on the second Allow nor the post-
	// sleep Allow lands inside the same bucket as a noise spike.
	// Earlier tunings (50/s = 20 ms window) flaked under -race on slow
	// runners because t.Parallel + race instrumentation pushes call
	// gaps past the refill window, so the throttled assertion fired
	// against an already-refilled bucket.
	rl, _, _ := newLimiter(t, admin.RateLimit{Capacity: 1, RefillPerSecond: 4})
	ctx := context.Background()
	if ok, _, _ := rl.Allow(ctx, "t", "s"); !ok {
		t.Fatal("first call must succeed")
	}
	if ok, _, _ := rl.Allow(ctx, "t", "s"); ok {
		t.Fatal("second call must throttle")
	}
	time.Sleep(350 * time.Millisecond) // > 250 ms refill window
	ok, _, err := rl.Allow(ctx, "t", "s")
	if err != nil {
		t.Fatalf("Allow after refill: %v", err)
	}
	if !ok {
		t.Fatal("call after refill must succeed")
	}
}

func TestRateLimiter_TenantIsolation(t *testing.T) {
	t.Parallel()
	rl, _, _ := newLimiter(t, admin.RateLimit{Capacity: 1, RefillPerSecond: 1})
	ctx := context.Background()
	if ok, _, _ := rl.Allow(ctx, "tenant-a", "src-1"); !ok {
		t.Fatal("tenant-a first call must succeed")
	}
	// tenant-b owns its own bucket.
	if ok, _, _ := rl.Allow(ctx, "tenant-b", "src-1"); !ok {
		t.Fatal("tenant-b must not share tenant-a's bucket")
	}
	// Same tenant, different source → independent bucket.
	if ok, _, _ := rl.Allow(ctx, "tenant-a", "src-2"); !ok {
		t.Fatal("source-scoped bucket must not collide with tenant-a/src-1")
	}
}

func TestRateLimiter_RejectsMissingIdentifiers(t *testing.T) {
	t.Parallel()
	rl, _, _ := newLimiter(t, admin.DefaultRateLimit)
	if _, _, err := rl.Allow(context.Background(), "", "s"); err == nil {
		t.Fatal("missing tenant must error")
	}
	if _, _, err := rl.Allow(context.Background(), "t", ""); err == nil {
		t.Fatal("missing source must error")
	}
}

func TestRateLimiter_BoundControllerWaitsThenAdmits(t *testing.T) {
	t.Parallel()
	// Refill 100/s → next token in 10ms.
	rl, _, _ := newLimiter(t, admin.RateLimit{Capacity: 1, RefillPerSecond: 100})
	ctx := context.Background()
	bc := &admin.BoundController{Limiter: rl, TenantID: "t", SourceID: "s"}
	if err := bc.Wait(ctx); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- bc.Wait(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait blocked forever")
	}
}

func TestRateLimiter_BoundControllerHonoursContextCancel(t *testing.T) {
	t.Parallel()
	rl, _, _ := newLimiter(t, admin.RateLimit{Capacity: 1, RefillPerSecond: 0.0001})
	bc := &admin.BoundController{Limiter: rl, TenantID: "t", SourceID: "s"}
	if err := bc.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := bc.Wait(ctx); err == nil {
		t.Fatal("expected ctx.Err()")
	}
}

// TestRateLimiter_Concurrent verifies the Lua script's atomicity
// across many concurrent goroutines on the same bucket.
func TestRateLimiter_Concurrent(t *testing.T) {
	t.Parallel()
	rl, _, _ := newLimiter(t, admin.RateLimit{Capacity: 5, RefillPerSecond: 0.0001})
	ctx := context.Background()
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ok, _, err := rl.Allow(ctx, "t", "s")
			if err != nil {
				return
			}
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 5 {
		t.Fatalf("expected exactly 5 admits across %d goroutines, got %d", goroutines, allowed)
	}
}
