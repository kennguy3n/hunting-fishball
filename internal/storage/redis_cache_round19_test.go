package storage_test

// redis_cache_round19_test.go — Round-19 Task 21.
//
// Behavioural tests for the cache-aside refresh and per-source
// tag-based invalidation primitives bolted onto SemanticCache.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func TestSemanticCache_InvalidateBySources_DropsOnlyMatchingEntries(t *testing.T) {
	t.Parallel()
	cache, _, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	// Two cached entries: one references source-A, one
	// references source-B. Invalidating source-A must leave
	// the source-B entry intact.
	queryA := []float32{0.1, 0.2}
	queryB := []float32{0.3, 0.4}
	resA := &storage.CachedResult{
		Hits: []storage.CachedHit{{ID: "c1", SourceID: "src-A", TenantID: "t"}},
	}
	resB := &storage.CachedResult{
		Hits: []storage.CachedHit{{ID: "c2", SourceID: "src-B", TenantID: "t"}},
	}
	if err := cache.Set(ctx, "t", "ch", queryA, "scope", resA, 30*time.Second); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if err := cache.Set(ctx, "t", "ch", queryB, "scope", resB, 30*time.Second); err != nil {
		t.Fatalf("Set B: %v", err)
	}
	if err := cache.InvalidateBySources(ctx, "t", []string{"src-A"}); err != nil {
		t.Fatalf("InvalidateBySources: %v", err)
	}
	gotA, _ := cache.Get(ctx, "t", "ch", queryA, "scope")
	if gotA != nil {
		t.Fatalf("source-A entry must be dropped, got %+v", gotA)
	}
	gotB, _ := cache.Get(ctx, "t", "ch", queryB, "scope")
	if gotB == nil {
		t.Fatal("source-B entry must survive InvalidateBySources(src-A)")
	}
}

func TestSemanticCache_InvalidateBySources_RejectsEmptyTenant(t *testing.T) {
	t.Parallel()
	cache, _, _ := newCacheWithMiniredis(t)
	if err := cache.InvalidateBySources(context.Background(), "", []string{"src"}); err == nil {
		t.Fatal("expected error when tenantID is empty")
	}
}

func TestSemanticCache_GetOrRefresh_ColdEntryTriggersRefresh(t *testing.T) {
	t.Parallel()
	cache, _, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	var calls int32
	refresh := func(_ context.Context) (*storage.CachedResult, error) {
		atomic.AddInt32(&calls, 1)
		return &storage.CachedResult{
			Hits: []storage.CachedHit{{ID: "c1", SourceID: "src-A", TenantID: "t"}},
		}, nil
	}
	got, err := cache.GetOrRefresh(ctx, "t", "ch", []float32{0.1}, "scope", time.Minute, 30*time.Second, refresh)
	if err != nil {
		t.Fatalf("GetOrRefresh: %v", err)
	}
	if got != nil {
		t.Fatalf("cold call must return nil, got %+v", got)
	}
	// Wait for the detached refresh goroutine to populate
	// the cache.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("refresh must be invoked on a cold cache")
	}
	// Subsequent Get should now see the refreshed entry.
	for i := 0; i < 50; i++ {
		fresh, _ := cache.Get(ctx, "t", "ch", []float32{0.1}, "scope")
		if fresh != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("refreshed entry never populated cache")
}

func TestSemanticCache_GetOrRefresh_FreshEntrySkipsRefresh(t *testing.T) {
	t.Parallel()
	cache, _, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	res := &storage.CachedResult{
		Hits: []storage.CachedHit{{ID: "c1", SourceID: "src-A", TenantID: "t"}},
	}
	if err := cache.Set(ctx, "t", "ch", []float32{0.1}, "scope", res, 30*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var calls int32
	refresh := func(_ context.Context) (*storage.CachedResult, error) {
		atomic.AddInt32(&calls, 1)
		return res, nil
	}
	got, err := cache.GetOrRefresh(ctx, "t", "ch", []float32{0.1}, "scope", time.Hour, 30*time.Second, refresh)
	if err != nil || got == nil {
		t.Fatalf("expected hit, got=%+v err=%v", got, err)
	}
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("fresh entry must not trigger refresh, got %d calls", calls)
	}
}

// TestSemanticCache_GetOrRefresh_DedupsConcurrentRefreshes pins the
// singleflight-based fix for the thundering-herd bug in GetOrRefresh:
// before the fix, N concurrent stale-readers spawned N independent
// refresh goroutines, all running the (expensive) RefreshFn and all
// racing to Set the same key. After the fix, the singleflight gate
// must collapse those N concurrent refreshes into a single
// invocation of RefreshFn per cache key per pass.
func TestSemanticCache_GetOrRefresh_DedupsConcurrentRefreshes(t *testing.T) {
	t.Parallel()
	cache, _, _ := newCacheWithMiniredis(t)
	ctx := context.Background()

	const callers = 50
	var calls int32
	// Hold every concurrent refresh inside RefreshFn until the test
	// signals release. This guarantees the singleflight window
	// captures every caller; without the gate, every blocked
	// goroutine would bump `calls`.
	release := make(chan struct{})
	refresh := func(_ context.Context) (*storage.CachedResult, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &storage.CachedResult{
			Hits: []storage.CachedHit{{ID: "c1", SourceID: "src-A", TenantID: "t"}},
		}, nil
	}

	// Fire `callers` concurrent GetOrRefresh calls. The entry is
	// cold, so every caller should observe the stale branch and
	// attempt to kick off the background refresh.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, _ = cache.GetOrRefresh(ctx, "t", "ch", []float32{0.1}, "scope", time.Minute, 30*time.Second, refresh)
		}()
	}
	close(start)
	wg.Wait()

	// Give the spawned background goroutines a moment to reach the
	// singleflight.Do call. Only one should actually enter
	// RefreshFn; the rest must attach to that in-flight call.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Wait long enough that any un-deduped goroutines would also
	// have bumped the counter.
	time.Sleep(200 * time.Millisecond)

	got := atomic.LoadInt32(&calls)
	close(release)
	if got != 1 {
		t.Fatalf("singleflight gate must collapse %d concurrent refreshes into a single RefreshFn invocation, got %d", callers, got)
	}

	// Confirm the cache eventually populates from the single
	// surviving refresh.
	for i := 0; i < 50; i++ {
		fresh, _ := cache.Get(ctx, "t", "ch", []float32{0.1}, "scope")
		if fresh != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("deduped refresh never populated the cache")
}
