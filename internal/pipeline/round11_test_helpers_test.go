package pipeline

// round11_test_helpers_test.go — shared helpers for the Round-11
// hook observability + timeout-guard tests. Kept in its own file
// so the assertions can read directly off the package-level
// Prometheus collectors without poking the registry from the test
// file itself.

import (
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// obsResetForTest is a thin trampoline so the Round-10 hook tests
// can call into the observability package without importing it at
// every site.
func obsResetForTest() {
	observability.ResetForTest()
}

// chunkQualityErrorCount returns the live value of
// context_engine_chunk_quality_errors_total.
func chunkQualityErrorCount() int {
	return int(testutil.ToFloat64(observability.ChunkQualityErrorsTotal))
}

// hookTimeoutCount returns the live value of
// context_engine_hook_timeouts_total{hook=label}.
func hookTimeoutCount(label string) int {
	return int(testutil.ToFloat64(observability.HookTimeoutsTotal.WithLabelValues(label)))
}
