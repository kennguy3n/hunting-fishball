package regression

// round910_manifest.go — Round-11 Task 3.
//
// Round-11 catalogues the Devin Review findings that landed during
// Round 9 (PR #18) and Round 10 (PR #19). Each entry below names
// the originating commit SHA + the regression test pinning the
// fix. Follows the same shape as Round78Manifest. Append-only.
//
// A meta-test in round910_manifest_test.go asserts every entry's
// regression-test file exists and is non-empty, so a rename or
// deletion of a regression test will fail CI immediately.

// Round910Manifest lists the Devin Review findings from PRs #18
// and #19, in commit order.
var Round910Manifest = []Bug{
	// ----- PR #18 (Round 9) Devin Review fixes --------------------

	{
		PR:      "18",
		Finding: 1,
		Title:   "recording-rules histogram suffix",
		Symptom: "deploy/recording-rules.yaml referenced context_engine_pipeline_stage_duration_ms_count, but the histogram is registered as ..._duration_seconds, so the recording rule was always empty and downstream dashboards/alerts saw no data",
		Tests: []TestRef{{
			PkgRel:   "deploy",
			TestName: "TestRecordingRulesManifest_Valid",
			Source:   "deploy/alerts_test.go",
		}},
	},
	{
		PR:      "18",
		Finding: 2,
		Title:   "recording-rules missing metric",
		Symptom: "context_engine_pipeline_stage_errors_total wasn't registered anywhere; the rule was re-based on context_engine_pipeline_retries_total{outcome=\"failed\"}, the canonical stage-level failure counter",
		Tests: []TestRef{{
			PkgRel:   "deploy",
			TestName: "TestRecordingRulesManifest_Valid",
			Source:   "deploy/alerts_test.go",
		}},
	},
	{
		PR:      "18",
		Finding: 3,
		Title:   "circuit breaker State() gauge",
		Symptom: "grpcpool.State() lazily transitioned Open -> HalfOpen on read but did not call publishState(), so the breaker_state gauge stayed at 2 (open) until the next allow() call",
		Tests: []TestRef{{
			PkgRel:   "internal/grpcpool",
			TestName: "TestStateLazyTransitionPublishesGauge",
			Source:   "internal/grpcpool/pool_test.go",
		}},
	},

	// ----- PR #19 (Round 10) Devin Review fixes -------------------

	{
		PR:      "19",
		Finding: 1,
		Title:   "Makefile fuzz multi-match",
		Symptom: "Makefile fuzz target used '-fuzz=. ./internal/retrieval/...' but Go's -fuzz flag rejects patterns that match more than one fuzz target per package; the nightly fuzz CI job would error out on both internal/retrieval (3 targets) and internal/admin (3 targets)",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "FuzzRetrieveRequestDecode",
			Source:   "internal/retrieval/handler_fuzz_test.go",
		}},
	},
	{
		PR:      "19",
		Finding: 2,
		Title:   "openapi GET vs POST mismatch",
		Symptom: "docs/openapi.yaml declared /v1/admin/isolation-check as GET, but isolation_audit.go registers POST and the unit tests + README all use POST; flipping the spec fixed the contract drift before clients regenerated stale SDKs",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestIsolationHandler_HTTP",
			Source:   "internal/admin/isolation_audit_test.go",
		}},
	},
	{
		PR:      "19",
		Finding: 3,
		Title:   "scheduler lifecycle drain",
		Symptom: "cmd/ingest/main.go allocated schedulerDone and closed it on scheduler exit, but never registered the channel with the lifecycle runner; the goroutine then raced Postgres/Redis teardown on shutdown, occasionally hanging the pod and producing a partial run",
		Tests: []TestRef{{
			PkgRel:   "internal/lifecycle",
			TestName: "TestShutdown_RunsStepsInOrder",
			Source:   "internal/lifecycle/shutdown_test.go",
		}},
	},
}
