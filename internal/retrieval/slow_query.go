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
	"strconv"
	"strings"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// ParseSlowQueryThreshold parses the
// CONTEXT_ENGINE_SLOW_QUERY_THRESHOLD_MS env-value as a
// non-negative integer. Returns (ms, true) on success; (0, false)
// on any parse error, negative value, or empty / whitespace-only
// input. Exposed for fuzz coverage in slow_query_fuzz_test.go.
func ParseSlowQueryThreshold(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	if v < 0 {
		return 0, false
	}
	return v, true
}

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
	observability.RetrievalSlowQueriesTotal.Inc()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
