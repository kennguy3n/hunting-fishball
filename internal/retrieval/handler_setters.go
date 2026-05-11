// handler_setters.go — Round-7 wiring hooks.
//
// Setters that let the cmd/api wiring layer attach admin-owned
// adapters (A/B router, query-analytics recorder, latency budget
// lookup, per-tenant cache TTL lookup, pin lookup) after the
// handler has been constructed. All are optional: tests and
// standalone deployments can skip them and the handler behaves
// identically to the Round-6 default.
//
// QueryAnalyticsRecorder is the narrow interface the retrieval
// path needs; the admin package's QueryAnalyticsRecorder
// satisfies it by name (duck typing, not import).
package retrieval

import (
	"context"
	"time"
)

// QueryAnalyticsRecorder is the call-site shape. Defined here
// rather than imported from internal/admin because retrieval is
// the leaf package — admin imports retrieval, never the other
// way around.
//
// The function shape (rather than an interface with a single
// method) lets the cmd/api wiring layer adapt admin's
// concrete recorder via a one-line closure without an extra
// type definition.
type QueryAnalyticsRecorder func(ctx context.Context, evt QueryAnalyticsEvent)

// QueryAnalyticsEvent is the package-neutral event shape. The
// admin recorder takes its own concrete type; the cmd/api
// wiring converts.
type QueryAnalyticsEvent struct {
	TenantID       string
	QueryText      string
	TopK           int
	HitCount       int
	CacheHit       bool
	LatencyMS      int
	BackendTimings map[string]int64
	ExperimentName string
	ExperimentArm  string
}

// SetABTestRouter swaps in (or replaces) the ABTestRouter the
// retrieval handler consults on each request. Pass a nil router
// to disable A/B routing.
func (h *Handler) SetABTestRouter(r ABTestRouter) {
	h.cfg.ABTests = r
}

// SetQueryAnalyticsRecorder swaps in (or replaces) the
// analytics recorder. The retrieval handler calls the
// recorder after every successful Retrieve.
func (h *Handler) SetQueryAnalyticsRecorder(r QueryAnalyticsRecorder) {
	h.cfg.QueryAnalytics = r
}

// LatencyBudgetLookup is the call-site shape for per-tenant
// latency budgets (Round-8 Task 9). Returns the configured
// budget and a "found" flag so the handler can fall back to the
// request's RequestedLatencyMS when no row exists.
type LatencyBudgetLookup func(ctx context.Context, tenantID string) (time.Duration, bool)

// SetLatencyBudgetLookup wires the admin-owned per-tenant
// latency budget store into the retrieval handler. The lookup
// runs on every Retrieve and bounds the request deadline so
// every backend fan-out completes within the tenant's budget.
func (h *Handler) SetLatencyBudgetLookup(fn LatencyBudgetLookup) {
	h.cfg.LatencyBudget = fn
}

// CacheTTLLookup is the call-site shape for per-tenant cache
// TTL overrides (Round-8 Task 10). Returns the TTL to use for
// the next Set; callers pass a fallback that is returned when no
// override exists.
type CacheTTLLookup func(ctx context.Context, tenantID string, fallback time.Duration) time.Duration

// SetCacheTTLLookup wires the admin-owned per-tenant cache TTL
// store into the retrieval handler's cache Set path.
func (h *Handler) SetCacheTTLLookup(fn CacheTTLLookup) {
	h.cfg.CacheTTLLookup = fn
}

// PinnedHit is the package-neutral pin shape that the admin
// store converts its rows into before the retrieval handler
// applies them.
type PinnedHit struct {
	ChunkID  string
	Position int
}

// PinLookup is the call-site shape for the admin-owned pinned
// retrieval results store (Round-8 Task 16). Returns the pins
// for the (tenant, query) pair; nil means no pins are active.
type PinLookup func(ctx context.Context, tenantID, query string) []PinnedHit

// SetPinLookup wires the admin-owned pinned-results store into
// the retrieval handler. The handler calls the lookup after
// policy filtering and before MMR diversity so pinned chunks
// land at their configured positions.
func (h *Handler) SetPinLookup(fn PinLookup) {
	h.cfg.PinLookup = fn
}

// recordQueryAnalytics emits one analytics event per retrieve
// response. The handler invokes this on every success path
// (cache hit, full fan-out). nil recorder is a no-op so tests
// that don't set one stay green.
func (h *Handler) recordQueryAnalytics(
	ctx context.Context,
	tenantID, queryText string,
	hits, topK int,
	cacheHit bool,
	start time.Time,
	backendTimings map[string]int64,
	route *ABTestRouteResult,
) {
	if h.cfg.QueryAnalytics == nil {
		return
	}
	evt := QueryAnalyticsEvent{
		TenantID:       tenantID,
		QueryText:      queryText,
		TopK:           topK,
		HitCount:       hits,
		CacheHit:       cacheHit,
		LatencyMS:      int(time.Since(start).Milliseconds()),
		BackendTimings: backendTimings,
	}
	if route != nil {
		evt.ExperimentName = route.ExperimentName
		evt.ExperimentArm = route.Arm
	}
	h.cfg.QueryAnalytics(ctx, evt)
}
