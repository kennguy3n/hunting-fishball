// connector_health_export_test.go — Round-22 test export.
//
// Exposes ConnectorHealthHandler.aggregate to tests in the
// admin_test package without leaking the in-memory aggregation
// helper into the public API.
package admin

import "context"

// AggregateForTesting returns the same payload the HTTP handler
// emits. It is exported only via the `_test.go` suffix so it
// disappears from the production binary.
func AggregateForTesting(ctx context.Context, h *ConnectorHealthHandler, tenantID string) (ConnectorHealthSummary, error) {
	return h.aggregate(ctx, tenantID)
}
