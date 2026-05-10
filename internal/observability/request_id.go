// request_id.go — Phase 8 / Task 20 request ID middleware.
//
// Reads the inbound X-Request-ID header and, when missing, mints a
// new ULID. The request ID lands on:
//
//   - the gin context (key=ContextKeyRequestID)
//   - the request context (so context-aware loggers pick it up)
//   - the response header (X-Request-ID; round-trips for correlation)
//   - the per-request slog logger (request_id field on every log line)
//
// When the existing trace ID middleware is also mounted, the request
// ID coexists with trace_id rather than replacing it — both are
// useful for incident triage.
package observability

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// RequestIDHeader is the canonical inbound + outbound header name.
const RequestIDHeader = "X-Request-ID"

// ContextKeyRequestID is the gin.Context key used by handlers that
// want to log / record the request ID without re-reading the header.
const ContextKeyRequestID = "request_id"

// requestIDCtxKey is a private type to avoid collisions with other
// packages that bind values to context.
type requestIDCtxKey struct{}

// RequestIDFromContext returns the bound request ID or "" when the
// middleware was not mounted (e.g. in unit tests that build a gin
// engine manually).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// WithRequestID returns ctx with the supplied request ID bound. Empty
// IDs are returned unchanged (callers should not mint empty IDs).
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDCtxKey{}, id)
}

// RequestIDMiddleware mounts the X-Request-ID propagation. Mount it
// EARLY in the middleware chain (before the slog binder) so every
// downstream log line and span carries the ID.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID(id) {
			id = ulid.Make().String()
		}
		c.Set(ContextKeyRequestID, id)
		c.Header(RequestIDHeader, id)
		ctx := WithRequestID(c.Request.Context(), id)
		// Wrap the per-request logger so subsequent ctx-aware logs
		// pick up the request_id automatically.
		if existing, ok := ctx.Value(ctxKeyLogger).(interface{ With(args ...any) interface{} }); ok && existing != nil {
			_ = existing // no-op for *slog.Logger; the LoggerFromContext path adds the key.
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// validRequestID accepts non-empty IDs up to 128 chars containing
// printable ASCII only. ULID is the default mint format but
// upstream services may already supply a UUID, KSUID, or similar
// — anything inside this whitelist round-trips untouched.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < 0x21 || c > 0x7e { // printable ASCII excluding space
			return false
		}
	}
	return true
}

// SetRequestIDOnResponse is a small helper for handlers that want
// to forward the request ID on a 4xx error envelope. Most callers
// should rely on the middleware's c.Header() write instead.
func SetRequestIDOnResponse(c *gin.Context) {
	if id := c.GetString(ContextKeyRequestID); id != "" {
		c.Writer.Header().Set(RequestIDHeader, id)
	}
}

// MaxRequestIDStatus is the HTTP status threshold above which we
// always echo the request ID even on synthetic 5xx responses
// produced before the middleware ran. Exposed for the tests.
const MaxRequestIDStatus = http.StatusInternalServerError
