// slow_query.go — Round-13 Task 8.
//
// Operators want a real-time signal when a retrieval crosses the
// configured slow-query threshold, plus a way to list every slow
// row in the recent past. The slow-row listing is handled by the
// admin endpoint (internal/admin/slow_query.go); this file holds
// the retrieval-side helper that emits the structured log entry.
package retrieval

import (
	"context"
	"log/slog"
)

// LogSlowQuery emits a single structured warn log entry per slow
// retrieval. The entry carries enough metadata for an operator to
// (1) reproduce the query and (2) see which backend was the long
// pole, without having to round-trip the analytics table.
func LogSlowQuery(_ context.Context, tenantID, queryText string, latencyMS int, backendTimings map[string]int64) {
	if tenantID == "" {
		return
	}
	attrs := []any{
		slog.String("tenant_id", tenantID),
		slog.String("query", truncate(queryText, 128)),
		slog.Int("latency_ms", latencyMS),
	}
	for backend, ms := range backendTimings {
		attrs = append(attrs, slog.Int64("backend_"+backend+"_ms", ms))
	}
	slog.Warn("retrieval: slow query", attrs...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
