package retrieval

// backend_timings_schema_test.go — Round-11 Devin Review Phase-5
// follow-up.
//
// The query_analytics.backend_timings JSONB column is written by
// two different functions:
//
//   - backendTimingsMillis: gin-handler fan-out path, keys come
//     directly from the Source* constants in merger.go
//     ("vector", "bm25", "graph", "memory").
//   - timingsToMap: RetrieveWithSnapshotCached path (batch +
//     cache-warm), projects the public RetrieveTimings struct.
//
// Both write into the same column, so any drift between their key
// sets puts query_analytics rows from the two paths in the same
// JSONB column under different schemas — operator dashboards that
// JOIN cache_hit + non-cache_hit rows on backend_timings.<backend>
// would silently miss the rows whose keys diverged.
//
// Pre-fix, timingsToMap emitted vector_ms / bm25_ms / graph_ms /
// memory_ms / merge_ms / rerank_ms (the JSON tags from
// RetrieveTimings) while backendTimingsMillis emitted vector /
// bm25 / graph / memory. This test pins both shapes to the same
// canonical set so future drift in either direction fails a
// build.

import (
	"sort"
	"testing"
	"time"
)

// TestBackendTimingsSchemaParity asserts that the two projection
// helpers that write the query_analytics.backend_timings JSONB
// column emit identical key sets, and that both match the
// canonical Source* constants from merger.go.
func TestBackendTimingsSchemaParity(t *testing.T) {
	t.Parallel()

	// canonical is the source of truth — the Source* constants
	// already used as the backend label across the codebase
	// (observability, recordTiming, hit.Source).
	canonical := []string{SourceVector, SourceBM25, SourceGraph, SourceMemory}
	sort.Strings(canonical)

	// backendTimingsMillis path: gin-handler fan-out builds a
	// map keyed by Source* and millisecond conversion is a
	// straight pass-through.
	ginPath := backendTimingsMillis(map[string]time.Duration{
		SourceVector: 12 * time.Millisecond,
		SourceBM25:   8 * time.Millisecond,
		SourceGraph:  3 * time.Millisecond,
		SourceMemory: 5 * time.Millisecond,
	})
	ginKeys := keysSorted(ginPath)

	// timingsToMap path: RetrieveWithSnapshotCached projects the
	// public RetrieveTimings struct (used on cache-hit and
	// cache-miss inside the batch + cache-warm flow).
	cachedPath := timingsToMap(RetrieveTimings{
		VectorMs: 12,
		BM25Ms:   8,
		GraphMs:  3,
		MemoryMs: 5,
		// Deliberately set MergeMs / RerankMs — these are pipeline
		// stages, not per-backend latency, and must NOT leak into
		// the per-backend map.
		MergeMs:  1,
		RerankMs: 1,
	})
	cachedKeys := keysSorted(cachedPath)

	if !stringSliceEqual(ginKeys, canonical) {
		t.Errorf("backendTimingsMillis key set %v != canonical %v — gin-handler fan-out path drifted from the Source* constants",
			ginKeys, canonical)
	}
	if !stringSliceEqual(cachedKeys, canonical) {
		t.Errorf("timingsToMap key set %v != canonical %v — RetrieveWithSnapshotCached path drifted; "+
			"query_analytics.backend_timings will carry two schemas, breaking dashboard JOINs across cache-hit/miss rows",
			cachedKeys, canonical)
	}
	if !stringSliceEqual(ginKeys, cachedKeys) {
		t.Errorf("schema parity broken: gin-fan-out keys %v vs cached-path keys %v — both functions write the same JSONB column and must emit identical key sets",
			ginKeys, cachedKeys)
	}

	// Sanity: per-stage timings (MergeMs / RerankMs) must NOT
	// leak into the per-backend map.
	for _, stage := range []string{"merge", "rerank", "merge_ms", "rerank_ms"} {
		if _, ok := cachedPath[stage]; ok {
			t.Errorf("timingsToMap emitted pipeline-stage key %q; the per-backend map is for backend latency only (Source* keys)", stage)
		}
	}
}

func keysSorted(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
