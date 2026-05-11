package storage_test

// redis_cache_tenantttl_test.go — Round-10 Task 2.
//
// The semantic cache exposes a TenantTTLLookup that lets the admin
// portal pin per-tenant TTLs (CacheTTLStore). The test confirms
// that two tenants with different configured TTLs end up with
// different PEXPIRE values on Redis, and that the lookup is
// consulted even when the call-site passes a non-zero ttl.

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func TestSemanticCache_TenantTTLLookup_PerTenantPEXPIRE(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	cache, err := storage.NewSemanticCache(storage.SemanticCacheConfig{
		Client:     rc,
		KeyPrefix:  "hf:",
		DefaultTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSemanticCache: %v", err)
	}

	// Two tenants with different configured TTLs.
	ttls := map[string]time.Duration{
		"tenant-fast": 2 * time.Second,
		"tenant-slow": 10 * time.Minute,
	}
	cache.SetTenantTTLLookup(func(_ context.Context, tenantID string, fallback time.Duration) time.Duration {
		if d, ok := ttls[tenantID]; ok {
			return d
		}
		return fallback
	})

	ctx := context.Background()
	res := &storage.CachedResult{Hits: []storage.CachedHit{{ID: "c", Score: 1.0}}}

	for tenant, want := range ttls {
		if err := cache.Set(ctx, tenant, "ch", []float32{0.1}, "scope", res, 0); err != nil {
			t.Fatalf("Set %s: %v", tenant, err)
		}
		key := cache.CacheKey(tenant, "ch", []float32{0.1}, "scope")
		got := mr.TTL(key)
		if got != want {
			t.Fatalf("tenant=%s want PEXPIRE %v got %v", tenant, want, got)
		}
	}
}

// TestSemanticCache_TenantTTLLookup_FallbackWhenMissing confirms
// that a tenant the lookup doesn't recognise falls through to the
// configured DefaultTTL (the lookup's `fallback` argument).
func TestSemanticCache_TenantTTLLookup_FallbackWhenMissing(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	const defaultTTL = 45 * time.Second
	cache, err := storage.NewSemanticCache(storage.SemanticCacheConfig{
		Client:     rc,
		KeyPrefix:  "hf:",
		DefaultTTL: defaultTTL,
	})
	if err != nil {
		t.Fatalf("NewSemanticCache: %v", err)
	}
	cache.SetTenantTTLLookup(func(_ context.Context, _ string, fallback time.Duration) time.Duration {
		return fallback
	})

	ctx := context.Background()
	res := &storage.CachedResult{Hits: []storage.CachedHit{{ID: "c", Score: 1.0}}}
	if err := cache.Set(ctx, "tenant-anon", "ch", []float32{0.1}, "scope", res, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	key := cache.CacheKey("tenant-anon", "ch", []float32{0.1}, "scope")
	if got := mr.TTL(key); got != defaultTTL {
		t.Fatalf("fallback TTL: want %v got %v", defaultTTL, got)
	}
}

// TestSemanticCache_TenantTTLLookup_OverridesCallSiteTTL confirms
// that the per-tenant override wins over a non-zero call-site ttl.
// This is the contract the cache-warm path relies on so admins can
// pin a tighter (or looser) tenant TTL without changing every
// caller.
func TestSemanticCache_TenantTTLLookup_OverridesCallSiteTTL(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	cache, err := storage.NewSemanticCache(storage.SemanticCacheConfig{
		Client:     rc,
		KeyPrefix:  "hf:",
		DefaultTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSemanticCache: %v", err)
	}
	const pinned = 7 * time.Second
	cache.SetTenantTTLLookup(func(_ context.Context, _ string, _ time.Duration) time.Duration {
		return pinned
	})

	ctx := context.Background()
	res := &storage.CachedResult{Hits: []storage.CachedHit{{ID: "c", Score: 1.0}}}
	if err := cache.Set(ctx, "tenant-a", "ch", []float32{0.1}, "scope", res, 99*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	key := cache.CacheKey("tenant-a", "ch", []float32{0.1}, "scope")
	if got := mr.TTL(key); got != pinned {
		t.Fatalf("override TTL: want %v got %v", pinned, got)
	}
}
