package storage

// Phase 3 semantic cache.
//
// The retrieval API consults this cache before the parallel fan-out
// (ARCHITECTURE.md §4.4). On hit the API returns immediately; on miss
// the API runs the fan-out and writes the merged + reranked results
// here. Stage 4 invalidates entries lazily — when a chunk that
// participated in a cached entry is rewritten, every entry that
// references that chunk is dropped.
//
// Cache key: SHA-256 over (tenant_id, channel_id, query_embedding,
// scope_hash) prefixed with a per-tenant key prefix. Cross-tenant
// reads are structurally impossible — the cache refuses to operate
// without a non-empty tenant_id (ErrMissingTenantScope) and prefixes
// every key with `<KeyPrefix>cache:<tenant_id>:`.
//
// Membership index: every cache entry also writes a `members:<chunk>`
// set listing the cache keys that reference the chunk; Invalidate
// walks those sets and deletes the matching entries so the next
// retrieval call re-fans the cold path.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// CachedHit is one entry within a CachedResult. It mirrors the public
// retrieval response so the cache can serialise the merged + reranked
// list directly without re-projecting through the handler.
type CachedHit struct {
	ID           string         `json:"id"`
	Score        float32        `json:"score"`
	TenantID     string         `json:"tenant_id"`
	SourceID     string         `json:"source_id,omitempty"`
	DocumentID   string         `json:"document_id,omitempty"`
	BlockID      string         `json:"block_id,omitempty"`
	Title        string         `json:"title,omitempty"`
	URI          string         `json:"uri,omitempty"`
	PrivacyLabel string         `json:"privacy_label,omitempty"`
	Text         string         `json:"text,omitempty"`
	Connector    string         `json:"connector,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// CachedResult is the cached response body for one retrieval request.
type CachedResult struct {
	Hits     []CachedHit `json:"hits"`
	CachedAt time.Time   `json:"cached_at"`
	ChunkIDs []string    `json:"chunk_ids,omitempty"`
}

// TenantTTLLookup returns the configured cache TTL for tenantID,
// or `fallback` when no override exists. Round-10 Task 2 wires
// this port into the cache itself so any caller (retrieval, cache
// warmer, batch handler, future surfaces) automatically gets the
// tenant's configured TTL without having to coordinate the
// lookup at the call site.
type TenantTTLLookup func(ctx context.Context, tenantID string, fallback time.Duration) time.Duration

// SemanticCacheConfig configures the semantic cache.
type SemanticCacheConfig struct {
	// Client is the Redis client. Required.
	Client redis.Cmdable

	// KeyPrefix is prepended to every cache key (e.g. "hf:" →
	// "hf:cache:<tenant_id>:<hash>"). Empty is allowed.
	KeyPrefix string

	// DefaultTTL is the fallback TTL when Set is called with ttl=0.
	// Defaults to 5 minutes.
	DefaultTTL time.Duration

	// TenantTTL is the optional per-tenant TTL lookup. When set,
	// the cache consults it on every Set with the call-site ttl as
	// the fallback (ttl=0 collapses to DefaultTTL first). Round-10
	// Task 2.
	TenantTTL TenantTTLLookup
}

// SemanticCache is the Redis-backed semantic cache.
type SemanticCache struct {
	cfg SemanticCacheConfig
}

// NewSemanticCache validates cfg and returns a *SemanticCache.
func NewSemanticCache(cfg SemanticCacheConfig) (*SemanticCache, error) {
	if cfg.Client == nil {
		return nil, errors.New("semantic-cache: nil Client")
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 5 * time.Minute
	}

	return &SemanticCache{cfg: cfg}, nil
}

// SetTenantTTLLookup swaps the per-tenant TTL lookup in place.
// Round-10 Task 2 wiring hook: cmd/api builds the cache before
// the admin GORM store is ready, so the lookup is attached after
// construction.
func (s *SemanticCache) SetTenantTTLLookup(fn TenantTTLLookup) {
	s.cfg.TenantTTL = fn
}

// resolveTTL collapses the call-site ttl, the configured default,
// and the per-tenant override into the final PEXPIRE the cache
// writes. Order is: (a) call-site ttl, (b) DefaultTTL, (c) the
// per-tenant lookup. Callers that supply a non-zero ttl still go
// through the lookup so an admin's tenant-pinned override always
// wins.
func (s *SemanticCache) resolveTTL(ctx context.Context, tenantID string, ttl time.Duration) time.Duration {
	if ttl == 0 {
		ttl = s.cfg.DefaultTTL
	}
	if s.cfg.TenantTTL != nil {
		return s.cfg.TenantTTL(ctx, tenantID, ttl)
	}
	return ttl
}

// CacheKey returns the canonical Redis key for a cache lookup. Exposed
// for tests and logs.
func (s *SemanticCache) CacheKey(tenantID, channelID string, queryEmbedding []float32, scopeHash string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(tenantID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(channelID))
	_, _ = h.Write([]byte{0})
	buf := make([]byte, 4)
	for _, v := range queryEmbedding {
		binary.LittleEndian.PutUint32(buf, mathFloat32Bits(v))
		_, _ = h.Write(buf)
	}
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(scopeHash))

	return fmt.Sprintf("%scache:%s:%s", s.cfg.KeyPrefix, tenantID, hex.EncodeToString(h.Sum(nil)))
}

// memberKey returns the per-chunk membership-set key. The set
// contains every cache key whose CachedResult includes the chunk.
func (s *SemanticCache) memberKey(tenantID, chunkID string) string {
	return fmt.Sprintf("%smembers:%s:%s", s.cfg.KeyPrefix, tenantID, chunkID)
}

// Get returns the cached result for the (tenant, channel, query) key.
// Returns (nil, nil) on cache miss; (result, nil) on hit.
func (s *SemanticCache) Get(ctx context.Context, tenantID, channelID string, queryEmbedding []float32, scopeHash string) (*CachedResult, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := s.CacheKey(tenantID, channelID, queryEmbedding, scopeHash)
	raw, err := s.cfg.Client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("semantic-cache: get: %w", err)
	}
	out := &CachedResult{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		// A corrupt entry isn't fatal; treat it as a miss and let
		// the caller refresh.
		return nil, nil
	}

	return out, nil
}

// Set writes the merged + reranked results into the cache and updates
// the per-chunk membership index. ttl=0 uses cfg.DefaultTTL.
func (s *SemanticCache) Set(ctx context.Context, tenantID, channelID string, queryEmbedding []float32, scopeHash string, result *CachedResult, ttl time.Duration) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if result == nil {
		return errors.New("semantic-cache: nil result")
	}

	key := s.CacheKey(tenantID, channelID, queryEmbedding, scopeHash)
	if result.CachedAt.IsZero() {
		result.CachedAt = time.Now().UTC()
	}
	if len(result.ChunkIDs) == 0 {
		result.ChunkIDs = chunkIDs(result.Hits)
	}

	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("semantic-cache: marshal: %w", err)
	}
	ttl = s.resolveTTL(ctx, tenantID, ttl)

	pipe := s.cfg.Client.Pipeline()
	pipe.Set(ctx, key, payload, ttl)
	for _, chunkID := range result.ChunkIDs {
		mk := s.memberKey(tenantID, chunkID)
		pipe.SAdd(ctx, mk, key)
		pipe.Expire(ctx, mk, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("semantic-cache: set: %w", err)
	}

	return nil
}

// Invalidate drops every cached entry that references any of the
// supplied chunk IDs. Used by Stage 4 after a chunk write so the next
// retrieval call sees fresh data.
func (s *SemanticCache) Invalidate(ctx context.Context, tenantID string, chunkIDs []string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunkIDs) == 0 {
		return nil
	}

	keys := map[string]struct{}{}
	for _, id := range chunkIDs {
		mk := s.memberKey(tenantID, id)
		members, err := s.cfg.Client.SMembers(ctx, mk).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("semantic-cache: smembers: %w", err)
		}
		for _, k := range members {
			keys[k] = struct{}{}
		}
		if err := s.cfg.Client.Del(ctx, mk).Err(); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("semantic-cache: del members: %w", err)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	args := make([]string, 0, len(keys))
	for k := range keys {
		args = append(args, k)
	}
	if err := s.cfg.Client.Del(ctx, args...).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("semantic-cache: del entries: %w", err)
	}

	return nil
}

// chunkIDs extracts the unique chunk IDs from the cached hits.
func chunkIDs(hits []CachedHit) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.ID == "" {
			continue
		}
		if _, ok := seen[h.ID]; ok {
			continue
		}
		seen[h.ID] = struct{}{}
		out = append(out, h.ID)
	}

	return out
}

// mathFloat32Bits returns the IEEE 754 bit pattern of f. Re-exported
// so the SHA-256 hash over the query embedding is independent of CPU
// endianness.
func mathFloat32Bits(f float32) uint32 {
	return math.Float32bits(f)
}
