// payload_limiter.go — Round-13 Task 11.
//
// PayloadSizeLimiter is a Gin middleware that rejects requests
// whose body exceeds CONTEXT_ENGINE_MAX_REQUEST_BODY_BYTES with
// HTTP 413 Payload Too Large. The probe (/healthz, /readyz,
// /metrics) and any GET / HEAD / DELETE routes are bypassed so an
// operator hitting /metrics is never accidentally throttled.
//
// The limiter looks at two signals:
//  1. Content-Length header — a fast pre-check that avoids
//     reading any of the body when the client honestly declares
//     an oversized payload.
//  2. http.MaxBytesReader — wraps Request.Body so a body that
//     lies about its length still trips the cap once the handler
//     attempts to read past the limit.
package observability

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// PayloadLimiterConfig configures the middleware.
type PayloadLimiterConfig struct {
	// MaxBytes is the upper bound on the request body. Bodies
	// strictly larger than this are rejected with 413. Setting
	// MaxBytes <= 0 disables the middleware (the returned
	// gin.HandlerFunc becomes a no-op).
	MaxBytes int64
	// SkipMethods lists HTTP methods that should never be
	// rate-limited. Defaults to GET, HEAD, DELETE if nil.
	SkipMethods []string
	// SkipPaths lists exact path strings that should never be
	// rate-limited. Probe endpoints fit here.
	SkipPaths []string
}

// PayloadSizeLimiter returns a Gin middleware enforcing cfg.
func PayloadSizeLimiter(cfg PayloadLimiterConfig) gin.HandlerFunc {
	if cfg.MaxBytes <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	skipMethods := map[string]struct{}{}
	if cfg.SkipMethods == nil {
		cfg.SkipMethods = []string{http.MethodGet, http.MethodHead, http.MethodDelete}
	}
	for _, m := range cfg.SkipMethods {
		skipMethods[strings.ToUpper(m)] = struct{}{}
	}
	skipPaths := map[string]struct{}{}
	for _, p := range cfg.SkipPaths {
		skipPaths[p] = struct{}{}
	}
	return func(c *gin.Context) {
		if _, ok := skipMethods[c.Request.Method]; ok {
			c.Next()
			return
		}
		if _, ok := skipPaths[c.Request.URL.Path]; ok {
			c.Next()
			return
		}
		if c.Request.ContentLength > cfg.MaxBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":          "payload too large",
				"max_bytes":      cfg.MaxBytes,
				"content_length": c.Request.ContentLength,
			})
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cfg.MaxBytes)
		}
		c.Next()
	}
}

// DefaultMaxRequestBodyBytes is the fallback cap (10 MiB) used
// when CONTEXT_ENGINE_MAX_REQUEST_BODY_BYTES is unset.
const DefaultMaxRequestBodyBytes int64 = 10 * 1024 * 1024

// PayloadSizeLimiterHTTP returns an http.Handler-style wrapper of
// the limiter for callers that don't use Gin (e.g. the ingest
// probe mux). The semantics match the Gin variant — oversized
// Content-Length yields HTTP 413, GET/HEAD/DELETE bypass, the
// body is wrapped with http.MaxBytesReader.
func PayloadSizeLimiterHTTP(cfg PayloadLimiterConfig, next http.Handler) http.Handler {
	if cfg.MaxBytes <= 0 {
		return next
	}
	skipMethods := map[string]struct{}{}
	if cfg.SkipMethods == nil {
		cfg.SkipMethods = []string{http.MethodGet, http.MethodHead, http.MethodDelete}
	}
	for _, m := range cfg.SkipMethods {
		skipMethods[strings.ToUpper(m)] = struct{}{}
	}
	skipPaths := map[string]struct{}{}
	for _, p := range cfg.SkipPaths {
		skipPaths[p] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := skipMethods[r.Method]; ok {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := skipPaths[r.URL.Path]; ok {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > cfg.MaxBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":"payload too large"}`))
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxBytes)
		}
		next.ServeHTTP(w, r)
	})
}
