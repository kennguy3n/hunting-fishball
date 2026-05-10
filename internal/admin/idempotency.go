// idempotency.go — Round-5 Task 17.
//
// IdempotencyMiddleware enforces the X-Idempotency-Key header on
// mutating admin requests. The first request with a given
// (tenant_id, method, path, key) tuple is executed normally and
// its full response (status + body + content-type) is captured
// into Redis with a 24h TTL. Any subsequent request that arrives
// before the TTL expires returns the cached response verbatim
// without re-executing the handler.
//
// Behaviour:
//
//   - Only POST/PATCH/PUT/DELETE are subject to caching. GET +
//     HEAD pass through unchanged.
//   - Requests without an X-Idempotency-Key header pass through
//     unchanged (clients opt in).
//   - Tenant scoping is mandatory — the cache key includes
//     tenant_id so a malicious caller cannot replay another
//     tenant's response.
//   - Falls open on Redis errors. We mirror the api_ratelimit
//     stance: failing closed would block legitimate traffic during
//     a Redis outage, while idempotency is a best-effort guard
//     that the application layer (DB constraints) is still
//     responsible for upholding.
//
// Cached payload format is stored as a single Redis string
// value: <status><LF><content-type><LF><body>. We avoid hashing
// algorithms here because the key already contains the SHA-256
// of (tenant_id|method|path|idempotency-key) and the Redis store
// is tenant-isolated.
package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// IdempotencyTTL is the cached-response retention window. 24h
// matches the canonical idempotency-key spec (RFC IETF draft).
const IdempotencyTTL = 24 * time.Hour

// IdempotencyHeader is the HTTP header clients set to opt-in.
const IdempotencyHeader = "X-Idempotency-Key"

// IdempotencyStore is the narrow Redis contract the middleware
// needs. *redis.Client satisfies it; tests inject a miniredis-
// backed client.
type IdempotencyStore interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) *redis.BoolCmd
}

// IdempotencyMiddlewareConfig configures the middleware.
type IdempotencyMiddlewareConfig struct {
	Store IdempotencyStore
	// TTL overrides IdempotencyTTL when set.
	TTL time.Duration
	// Prefix is prepended to every cache key. Defaults to
	// "idempotency".
	Prefix string
}

// NewIdempotencyMiddleware validates cfg and returns the Gin
// middleware.
func NewIdempotencyMiddleware(cfg IdempotencyMiddlewareConfig) (gin.HandlerFunc, error) {
	if cfg.Store == nil {
		return nil, errors.New("idempotency: nil Store")
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = IdempotencyTTL
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "idempotency"
	}
	return func(c *gin.Context) {
		if !idempotencyApplies(c.Request.Method) {
			c.Next()
			return
		}
		key := strings.TrimSpace(c.GetHeader(IdempotencyHeader))
		if key == "" {
			c.Next()
			return
		}
		tenantID := tenantIDFromContextOptional(c)
		cacheKey := buildIdempotencyKey(prefix, tenantID, c.Request.Method, c.Request.URL.Path, key)

		// Cache hit short-circuits the handler.
		if cached, ok := readCachedResponse(c.Request.Context(), cfg.Store, cacheKey); ok {
			c.Header("X-Idempotency-Replayed", "true")
			if cached.contentType != "" {
				c.Header("Content-Type", cached.contentType)
			}
			c.Status(cached.status)
			_, _ = c.Writer.Write(cached.body)
			c.Abort()
			return
		}

		// Cache miss: capture writes via a wrapper, run the
		// handler, then SetNX the captured response. SetNX (not
		// SET) prevents concurrent writers from racing the cache
		// — second writer's SetNX returns 0 and we silently drop
		// the duplicate write.
		w := &capturingResponseWriter{ResponseWriter: c.Writer, body: &bytes.Buffer{}, status: http.StatusOK}
		c.Writer = w
		c.Next()
		w.flush()

		if w.status >= 200 && w.status < 300 {
			payload := encodeCachedResponse(w.status, c.Writer.Header().Get("Content-Type"), w.body.Bytes())
			_ = cfg.Store.SetNX(c.Request.Context(), cacheKey, payload, ttl).Err()
		}
	}, nil
}

// idempotencyApplies returns true for HTTP methods that mutate
// state. GET / HEAD / OPTIONS pass through unchanged because
// they're already idempotent at the HTTP layer.
func idempotencyApplies(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// tenantIDFromContextOptional returns "" instead of (false) when
// the tenant is missing — idempotency middleware mounts on the
// authenticated /v1/admin group so a missing tenant means the
// request was ill-formed and we let the handler reject it.
func tenantIDFromContextOptional(c *gin.Context) string {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// buildIdempotencyKey hashes the tuple so the Redis key has
// fixed length regardless of header content.
func buildIdempotencyKey(prefix, tenantID, method, path, key string) string {
	h := sha256.New()
	h.Write([]byte(tenantID))
	h.Write([]byte{0})
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return prefix + ":" + hex.EncodeToString(h.Sum(nil))
}

type cachedResponse struct {
	status      int
	contentType string
	body        []byte
}

func encodeCachedResponse(status int, contentType string, body []byte) []byte {
	out := make([]byte, 0, len(body)+64)
	out = append(out, []byte(strconv.Itoa(status))...)
	out = append(out, '\n')
	out = append(out, []byte(contentType)...)
	out = append(out, '\n')
	out = append(out, body...)
	return out
}

func decodeCachedResponse(b []byte) (cachedResponse, bool) {
	if len(b) == 0 {
		return cachedResponse{}, false
	}
	first := bytes.IndexByte(b, '\n')
	if first <= 0 {
		return cachedResponse{}, false
	}
	status, err := strconv.Atoi(string(b[:first]))
	if err != nil {
		return cachedResponse{}, false
	}
	rest := b[first+1:]
	second := bytes.IndexByte(rest, '\n')
	if second < 0 {
		return cachedResponse{}, false
	}
	return cachedResponse{
		status:      status,
		contentType: string(rest[:second]),
		body:        rest[second+1:],
	}, true
}

func readCachedResponse(ctx context.Context, store IdempotencyStore, key string) (cachedResponse, bool) {
	raw, err := store.Get(ctx, key).Bytes()
	if err != nil {
		return cachedResponse{}, false
	}
	return decodeCachedResponse(raw)
}

// capturingResponseWriter intercepts the body so we can cache it
// after the handler runs. Status capture is needed because Gin
// only assigns response.WriteHeader implicitly on first write
// when the handler doesn't call c.Status() explicitly.
type capturingResponseWriter struct {
	gin.ResponseWriter
	body    *bytes.Buffer
	status  int
	flushed bool
}

func (w *capturingResponseWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *capturingResponseWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

func (w *capturingResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *capturingResponseWriter) flush() {
	if w.flushed {
		return
	}
	w.flushed = true
	if w.status == 0 {
		w.status = w.ResponseWriter.Status()
	}
}
