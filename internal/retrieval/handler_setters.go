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
	// Source tags the call site (Round-11 Task 9). One of
	// QueryAnalyticsSource{User, CacheWarm, Batch}. Empty defaults
	// to "user" downstream.
	Source string
	// Slow — Round-13 Task 8. Set by the retrieval handler when
	// LatencyMS exceeded the configured slow-query threshold so
	// the persisted row carries the `slow:true` flag.
	Slow bool
}

// Source enum mirrored from internal/admin so callers can pass
// constants without importing the admin package (which would form
// a cycle: retrieval -> admin -> retrieval). Values are pinned
// here and asserted equal via TestQueryAnalyticsSourceConstants.
const (
	QueryAnalyticsSourceUser      = "user"
	QueryAnalyticsSourceCacheWarm = "cache_warm"
	QueryAnalyticsSourceBatch     = "batch"
)

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

// SetQueryExpander swaps in (or replaces) the per-tenant query
// expander (Round-10 Task 9). The expander runs ahead of the
// BM25 / memory fan-out so the expanded form is what hits the
// backend search adapters.
func (h *Handler) SetQueryExpander(qe QueryExpander) {
	h.cfg.QueryExpander = qe
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
	source string,
) {
	if h.cfg.QueryAnalytics == nil {
		return
	}
	if source == "" {
		source = QueryAnalyticsSourceUser
	}
	latencyMS := int(time.Since(start).Milliseconds())
	// Round-13 Task 8: flag the row when it crossed the
	// per-deployment slow-query threshold so operators can list
	// it from /v1/admin/analytics/queries/slow without scanning
	// every row. Also emit a structured warn log so operators
	// see the slow event in real-time alongside the per-backend
	// timings.
	slow := false
	threshold := h.cfg.SlowQueryThresholdMS
	if threshold > 0 && latencyMS >= threshold {
		slow = true
		LogSlowQuery(ctx, tenantID, queryText, latencyMS, backendTimings)
	}
	evt := QueryAnalyticsEvent{
		TenantID:       tenantID,
		QueryText:      queryText,
		TopK:           topK,
		HitCount:       hits,
		CacheHit:       cacheHit,
		LatencyMS:      latencyMS,
		BackendTimings: backendTimings,
		Source:         source,
		Slow:           slow,
	}
	if route != nil {
		evt.ExperimentName = route.ExperimentName
		evt.ExperimentArm = route.Arm
	}
	h.cfg.QueryAnalytics(ctx, evt)
}
