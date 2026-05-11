// handler_setters.go — Round-7 wiring hooks.
//
// Two setters that let the cmd/api wiring layer attach the
// admin-owned ABTestRouter adapter and a query-analytics
// recorder after the handler has been constructed. Both are
// optional: tests and standalone deployments can skip them and
// the handler behaves identically to the Round-6 default.
//
// QueryAnalyticsRecorder is the narrow interface the retrieval
// path needs; the admin package's QueryAnalyticsRecorder
// satisfies it by name (duck typing, not import).
package retrieval

import "context"

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
