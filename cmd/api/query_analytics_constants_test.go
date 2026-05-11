package main

// query_analytics_constants_test.go — Round-11 Devin Review
// Phase-3 follow-up.
//
// The QueryAnalyticsSource* constants are duplicated between
// internal/retrieval/handler_setters.go and
// internal/admin/query_analytics.go because the import graph runs
// admin -> retrieval (so retrieval cannot import admin without a
// cycle). The duplication is annotated on the retrieval side as
// "Values are pinned here and asserted equal via
// TestQueryAnalyticsSourceConstants" — this is that test.
//
// The admin recorder normalises unknown source values back to
// QueryAnalyticsSourceUser (see
// internal/admin/query_analytics.go's Record path), so a silent
// drift between the two packages would misclassify batch +
// cache-warm traffic as organic user traffic in the
// `query_analytics` rows without any compile-time signal. We
// pin both packages against a third side — the literal expected
// strings — so the test fails even if both packages drift in the
// same direction (e.g. both flip to "organic").
//
// cmd/api is the only package that imports BOTH internal/admin
// and internal/retrieval today, so this is the natural home for
// the assertion without forming an import cycle.

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

func TestQueryAnalyticsSourceConstants(t *testing.T) {
	cases := []struct {
		name      string
		retrieval string
		admin     string
		want      string
	}{
		{
			name:      "user",
			retrieval: retrieval.QueryAnalyticsSourceUser,
			admin:     admin.QueryAnalyticsSourceUser,
			want:      "user",
		},
		{
			name:      "cache_warm",
			retrieval: retrieval.QueryAnalyticsSourceCacheWarm,
			admin:     admin.QueryAnalyticsSourceCacheWarm,
			want:      "cache_warm",
		},
		{
			name:      "batch",
			retrieval: retrieval.QueryAnalyticsSourceBatch,
			admin:     admin.QueryAnalyticsSourceBatch,
			want:      "batch",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.retrieval != tc.admin {
				t.Fatalf("retrieval (%q) != admin (%q) — duplicated QueryAnalyticsSource constants drifted; the admin recorder would normalise the mismatched value back to %q and misclassify the traffic",
					tc.retrieval, tc.admin, admin.QueryAnalyticsSourceUser)
			}
			if tc.retrieval != tc.want {
				t.Fatalf("retrieval QueryAnalyticsSource%s = %q, want %q — value drifted from the expected wire format used by the migrations / analytics rollups",
					tc.name, tc.retrieval, tc.want)
			}
			if tc.admin != tc.want {
				t.Fatalf("admin QueryAnalyticsSource%s = %q, want %q — value drifted from the expected wire format used by the migrations / analytics rollups",
					tc.name, tc.admin, tc.want)
			}
		})
	}
}
