// Package observability — logger.go wraps Go's stdlib log/slog
// (Go 1.21+) with a JSON handler whose every line carries the
// canonical context-engine telemetry envelope: timestamp, level,
// component, tenant_id, trace_id, msg.
//
// Usage:
//
//	log := observability.NewLogger("api")
//	log.Info("request started", slog.String("path", "/v1/retrieve"))
//
// In a Gin handler, prefer LoggerFromContext to pick up the per-
// request tenant_id + trace_id that the GinLoggerMiddleware injects
// into the request context. Falling back to NewLogger is fine when
// no request context is available.
//
// The logger is a thin layer over slog.JSONHandler; we deliberately
// do not wrap with another structured-logging library so callers
// can mix-in zerolog/zap/logr-style sinks via slog.NewLogLogger if
// they ever need to.
package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type ctxKey int

const (
	ctxKeyLogger ctxKey = iota // *slog.Logger handle.
	ctxKeyTenant               // string tenant_id.
	ctxKeyTrace                // string trace_id.
)

// defaultLogger is the package-level fallback returned by
// LoggerFromContext when the context has no logger of its own.
// Reads/writes are guarded by defaultMu so SetDefault can be called
// concurrently with logging during graceful reconfiguration.
var (
	defaultMu     sync.RWMutex
	defaultLogger *slog.Logger
)

// NewLogger returns a JSON-handler slog.Logger pre-bound with the
// supplied component name. Component is a stable identifier the
// operator can grep for (e.g. "api", "ingest", "ingest.dlq",
// "shard.forget"). The handler writes to os.Stderr — Kubernetes
// log shippers tail it without further configuration.
func NewLogger(component string) *slog.Logger {
	return NewLoggerWithWriter(os.Stderr, component)
}

// NewLoggerWithWriter is the same as NewLogger but writes to the
// supplied io.Writer. Tests use this to capture the JSON output and
// assert on the line shape.
func NewLoggerWithWriter(w io.Writer, component string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	return slog.New(handler).With(slog.String("component", component))
}

// SetDefault wires the supplied logger as the package-level fallback
// returned by LoggerFromContext when the context has no logger of
// its own. Returns the previous default so callers can restore it
// (used by cmd/api and cmd/ingest in their main() initialisation).
func SetDefault(l *slog.Logger) *slog.Logger {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	prev := defaultLogger
	defaultLogger = l
	return prev
}

// fallbackLogger returns the lazily-initialised package logger. We
// intentionally do not auto-wire SetDefault from init() so test
// binaries can capture the logger via NewLoggerWithWriter without
// depending on the real os.Stderr handle.
func fallbackLogger() *slog.Logger {
	defaultMu.RLock()
	if defaultLogger != nil {
		defer defaultMu.RUnlock()
		return defaultLogger
	}
	defaultMu.RUnlock()
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultLogger == nil {
		defaultLogger = NewLogger("context-engine")
	}
	return defaultLogger
}

// WithLogger returns a new context carrying logger. Used by the Gin
// middleware before each request handler runs.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, logger)
}

// WithTenantID returns a new context with the supplied tenant_id
// bound. The middleware sets this when the auth layer resolves
// the tenant; downstream handlers pick it up via LoggerFromContext.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	if tenantID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyTenant, tenantID)
}

// WithTraceID returns a new context with the supplied trace_id
// bound. Trace IDs propagate from the inbound `traceparent` header
// or, when absent, the middleware mints a random one.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyTrace, traceID)
}

// TenantIDFromContext returns the bound tenant_id, or "".
func TenantIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTenant).(string); ok {
		return v
	}
	return ""
}

// TraceIDFromContext returns the bound trace_id, or "".
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTrace).(string); ok {
		return v
	}
	return ""
}

// LoggerFromContext returns the slog logger bound to ctx, or the
// package-level fallback if none is bound. The returned logger is
// always pre-decorated with the tenant_id and trace_id keys (when
// they exist on the context) so callers don't have to remember to
// stamp them on every line.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	var base *slog.Logger
	if v, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && v != nil {
		base = v
	} else {
		base = fallbackLogger()
	}
	attrs := make([]any, 0, 4)
	if t := TenantIDFromContext(ctx); t != "" {
		attrs = append(attrs, slog.String("tenant_id", t))
	}
	if tr := TraceIDFromContext(ctx); tr != "" {
		attrs = append(attrs, slog.String("trace_id", tr))
	}
	if len(attrs) == 0 {
		return base
	}
	return base.With(attrs...)
}

// GinLoggerMiddleware injects a per-request slog logger plus the
// canonical telemetry keys into the Gin request context. Mount it
// AFTER the auth middleware so the tenant_id is already resolved on
// the gin.Context — it's read from the same TenantContextKey the
// audit/admin packages already use.
//
// Trace IDs follow the W3C `traceparent` header when present; we
// pull only the trace-id portion and ignore parent-span / flags so
// the per-request log is correlatable with whichever upstream sent
// the header.
func GinLoggerMiddleware(component string) gin.HandlerFunc {
	logger := NewLogger(component)
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		ctx = WithLogger(ctx, logger)

		// Tenant ID: read from the audit/admin context key the rest
		// of the API uses. Falls back to a header for binaries that
		// don't run an auth layer.
		if tenantVal, ok := c.Get(audit.TenantContextKey); ok {
			if t, _ := tenantVal.(string); t != "" {
				ctx = WithTenantID(ctx, t)
			}
		} else if h := c.GetHeader("X-Tenant-Id"); h != "" {
			ctx = WithTenantID(ctx, h)
		}

		// Trace ID: prefer the W3C `traceparent` header.
		if tp := c.GetHeader("traceparent"); tp != "" {
			ctx = WithTraceID(ctx, parseTraceID(tp))
		} else if h := c.GetHeader("X-Trace-Id"); h != "" {
			ctx = WithTraceID(ctx, h)
		}

		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// parseTraceID extracts the 32-hex trace-id from a W3C traceparent
// header. The format is `00-<trace-id>-<span-id>-<flags>`. Returns
// "" when the header is malformed; callers fall back to whatever
// trace ID the inbound infra synthesized.
func parseTraceID(header string) string {
	// Quick parser to avoid pulling in a heavyweight tracing dep
	// when we only need one field.
	const wantLen = 55 // 2 + 1 + 32 + 1 + 16 + 1 + 2
	if len(header) < wantLen {
		return ""
	}
	if header[2] != '-' || header[35] != '-' {
		return ""
	}
	return header[3:35]
}
