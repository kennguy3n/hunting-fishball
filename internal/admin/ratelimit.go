// Package admin's rate limiter is a per-(tenant, source) token bucket
// backed by Redis. The backfill orchestrator calls Wait before
// emitting each event so connectors don't hammer external APIs faster
// than their tenant-configured budget.
//
// Algorithm: an atomic Redis Lua script tracks two keys per
// (tenant, source) — `tokens` and `last_refill`. Each call refills
// tokens proportional to elapsed time (capped at the bucket capacity)
// and consumes one token. When the bucket is empty the script returns
// the millisecond delay until the next token is available, and the
// caller sleeps before retrying.
//
// Persisting state in Redis (rather than in-process) is required so
// the limit holds across the multi-replica backend — every fetch
// goroutine in every replica funnels through the same bucket.
package admin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCmd is the narrow contract the limiter needs from go-redis.
// *redis.Client satisfies it; tests inject a miniredis-backed client.
type RedisCmd interface {
	EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *redis.Cmd
	ScriptLoad(ctx context.Context, script string) *redis.StringCmd
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
}

// RateLimit configures a token bucket. Use DefaultRateLimit unless
// the source-level config in Postgres overrides it.
type RateLimit struct {
	// Capacity is the maximum number of tokens the bucket can hold.
	// Bursts up to Capacity are allowed.
	Capacity int

	// RefillPerSecond is the steady-state token replenishment rate.
	// Effective sustained throughput is RefillPerSecond requests/sec.
	RefillPerSecond float64
}

// DefaultRateLimit is a conservative default for a freshly-connected
// source. cmd/api/main.go overrides via env vars.
var DefaultRateLimit = RateLimit{
	Capacity:        100,
	RefillPerSecond: 10,
}

// RateLimiter is the per-source token bucket. Construct with
// NewRateLimiter; Wait blocks until a token is available.
type RateLimiter struct {
	client    RedisCmd
	keyPrefix string
	limit     RateLimit
	scriptSHA string
}

// RateLimiterConfig configures a RateLimiter.
type RateLimiterConfig struct {
	Client    RedisCmd
	KeyPrefix string // namespace, e.g. "hf:rl"
	Limit     RateLimit
}

// NewRateLimiter loads the Lua script into Redis and returns a
// limiter. Returns an error when the script can't be loaded.
func NewRateLimiter(ctx context.Context, cfg RateLimiterConfig) (*RateLimiter, error) {
	if cfg.Client == nil {
		return nil, errors.New("ratelimit: nil Client")
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "hf:rl"
	}
	if cfg.Limit.Capacity <= 0 || cfg.Limit.RefillPerSecond <= 0 {
		cfg.Limit = DefaultRateLimit
	}
	sha, err := cfg.Client.ScriptLoad(ctx, tokenBucketLua).Result()
	if err != nil {
		return nil, fmt.Errorf("ratelimit: load script: %w", err)
	}
	return &RateLimiter{
		client: cfg.Client, keyPrefix: cfg.KeyPrefix,
		limit: cfg.Limit, scriptSHA: sha,
	}, nil
}

// Allow consumes one token if available. Returns (allowed, retryAfter).
// retryAfter is the wall-clock duration the caller should sleep before
// retrying when allowed is false.
func (r *RateLimiter) Allow(ctx context.Context, tenantID, sourceID string) (bool, time.Duration, error) {
	if tenantID == "" || sourceID == "" {
		return false, 0, errors.New("ratelimit: missing tenant/source")
	}
	key := fmt.Sprintf("%s:%s:%s", r.keyPrefix, tenantID, sourceID)
	now := time.Now().UnixMilli()
	args := []any{
		strconv.Itoa(r.limit.Capacity),
		strconv.FormatFloat(r.limit.RefillPerSecond, 'f', -1, 64),
		strconv.FormatInt(now, 10),
	}

	// Try cached SHA first; fall back to inline Eval on NOSCRIPT.
	cmd := r.client.EvalSha(ctx, r.scriptSHA, []string{key}, args...)
	res, err := cmd.Result()
	if err != nil {
		// miniredis returns the inline-Eval path; production Redis can
		// evict the script after FLUSH and respond with NOSCRIPT.
		cmd = r.client.Eval(ctx, tokenBucketLua, []string{key}, args...)
		res, err = cmd.Result()
		if err != nil {
			return false, 0, fmt.Errorf("ratelimit: eval: %w", err)
		}
	}

	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return false, 0, fmt.Errorf("ratelimit: unexpected reply: %T %v", res, res)
	}
	allowed, _ := arr[0].(int64)
	retryMs, _ := arr[1].(int64)
	return allowed == 1, time.Duration(retryMs) * time.Millisecond, nil
}

// Wait blocks until a token is available or ctx is cancelled. It
// makes the limiter directly usable as a pipeline.RateController.
func (r *RateLimiter) Wait(ctx context.Context) error {
	// Wait on a default key — most callers wrap this in a closure that
	// pins (tenant, source). See WaitFor below.
	return errors.New("ratelimit: use WaitFor(ctx, tenantID, sourceID) — Wait is not bucket-aware")
}

// WaitFor blocks until a token is available for (tenantID, sourceID)
// or ctx is cancelled.
func (r *RateLimiter) WaitFor(ctx context.Context, tenantID, sourceID string) error {
	for {
		allowed, retry, err := r.Allow(ctx, tenantID, sourceID)
		if err != nil {
			return err
		}
		if allowed {
			return nil
		}
		if retry <= 0 {
			retry = 50 * time.Millisecond
		}
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// BoundController wraps a RateLimiter into a pipeline.RateController
// pinned to a single (tenant, source) pair. Backfill orchestrators
// instantiate one per source.
type BoundController struct {
	Limiter  *RateLimiter
	TenantID string
	SourceID string
}

// Wait satisfies pipeline.RateController.
func (b *BoundController) Wait(ctx context.Context) error {
	return b.Limiter.WaitFor(ctx, b.TenantID, b.SourceID)
}

// tokenBucketLua atomically refills the bucket and either consumes a
// token or returns the retry-after delay. The script is the standard
// Redis token-bucket pattern. Returns {allowed, retry_after_ms}.
const tokenBucketLua = `
local key       = KEYS[1]
local capacity  = tonumber(ARGV[1])
local refill    = tonumber(ARGV[2])
local now_ms    = tonumber(ARGV[3])

local data    = redis.call('HMGET', key, 'tokens', 'last')
local tokens  = tonumber(data[1])
local last    = tonumber(data[2])

if not tokens then tokens = capacity end
if not last   then last   = now_ms   end

local elapsed = (now_ms - last) / 1000.0
if elapsed < 0 then elapsed = 0 end
tokens = math.min(capacity, tokens + elapsed * refill)

local allowed
local retry_ms
if tokens >= 1 then
    tokens   = tokens - 1
    allowed  = 1
    retry_ms = 0
else
    allowed  = 0
    -- ceil((1 - tokens) / refill * 1000)
    retry_ms = math.ceil((1 - tokens) / refill * 1000)
end

redis.call('HSET',    key, 'tokens', tokens, 'last', now_ms)
-- TTL keeps idle buckets from accumulating; choose 1h.
redis.call('PEXPIRE', key, 3600000)

return { allowed, retry_ms }
`

// RateLimitStatus is a read-only snapshot of the bucket state for
// (tenantID, sourceID). Round-12 Task 16. The fields mirror the
// JSON payload returned by GET /v1/admin/sources/:id/rate-limit-status:
//
//   - CurrentTokens is the most recently observed token count for
//     the bucket. May be slightly stale because Redis updates the
//     bucket only on Allow() calls; the time-decay refill is
//     computed at the moment of Inspect.
//   - MaxTokens mirrors the configured Capacity.
//   - EffectiveRate is the steady-state refill rate (tokens/sec)
//     after any adaptive halving. NB: the admin's RateLimiter is a
//     simple Lua bucket without per-key adaptive state, so this
//     defers to the configured RefillPerSecond and the
//     AdaptiveRateLimiter wrapper sets the actual rate on Redis.
//   - HalveCount and Last429At require external bookkeeping (the
//     AdaptiveRateLimiter); see RateLimitStatusFromAdaptive.
//   - IsThrottled is true when CurrentTokens == 0.
type RateLimitStatus struct {
	TenantID      string    `json:"tenant_id"`
	SourceID      string    `json:"source_id"`
	CurrentTokens float64   `json:"current_tokens"`
	MaxTokens     int       `json:"max_tokens"`
	EffectiveRate float64   `json:"effective_rate"`
	HalveCount    int       `json:"halve_count"`
	Last429At     time.Time `json:"last_429_at,omitempty"`
	IsThrottled   bool      `json:"is_throttled"`
}

// inspectLua reads the bucket state without consuming a token. It
// mirrors the time-decay refill from tokenBucketLua so the
// returned CurrentTokens reflects the value Allow would see.
const inspectLua = `
local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill   = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])

local data   = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(data[1])
local last   = tonumber(data[2])

if not tokens then tokens = capacity end
if not last   then last   = now_ms   end

local elapsed = (now_ms - last) / 1000.0
if elapsed < 0 then elapsed = 0 end
tokens = math.min(capacity, tokens + elapsed * refill)

return tostring(tokens)
`

// Inspect returns the current bucket snapshot for (tenantID,
// sourceID) without consuming a token. Used by the
// /v1/admin/sources/:id/rate-limit-status endpoint.
func (r *RateLimiter) Inspect(ctx context.Context, tenantID, sourceID string) (RateLimitStatus, error) {
	if tenantID == "" || sourceID == "" {
		return RateLimitStatus{}, errors.New("ratelimit: missing tenant/source")
	}
	key := fmt.Sprintf("%s:%s:%s", r.keyPrefix, tenantID, sourceID)
	now := time.Now().UnixMilli()
	args := []any{
		strconv.Itoa(r.limit.Capacity),
		strconv.FormatFloat(r.limit.RefillPerSecond, 'f', -1, 64),
		strconv.FormatInt(now, 10),
	}
	// We always Eval (no caching) because Inspect is on the slow
	// admin path; the script is small and the cost is negligible.
	res, err := r.client.Eval(ctx, inspectLua, []string{key}, args...).Result()
	if err != nil {
		return RateLimitStatus{}, fmt.Errorf("ratelimit: inspect: %w", err)
	}
	str, ok := res.(string)
	if !ok {
		return RateLimitStatus{}, fmt.Errorf("ratelimit: inspect: unexpected reply: %T", res)
	}
	tokens, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return RateLimitStatus{}, fmt.Errorf("ratelimit: inspect parse: %w", err)
	}
	return RateLimitStatus{
		TenantID:      tenantID,
		SourceID:      sourceID,
		CurrentTokens: tokens,
		MaxTokens:     r.limit.Capacity,
		EffectiveRate: r.limit.RefillPerSecond,
		IsThrottled:   tokens < 1,
	}, nil
}
