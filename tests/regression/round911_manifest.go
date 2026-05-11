package regression

// round911_manifest.go — Round-12 Task 18.
//
// Round-12 catalogues the Devin Review findings that landed during
// Round 11 (PR #20). The Round 9/10 findings already live in
// Round910Manifest; this manifest extends the coverage forward.
//
// Append-only. A meta-test in round911_manifest_test.go asserts
// every entry's regression-test exists in tree (same shape as
// Round78 / Round910).

// Round911Manifest lists the Devin Review findings from PR #20.
var Round911Manifest = []Bug{
	// ----- PR #20 (Round 11) Devin Review fixes ------------------

	{
		PR:      "20",
		Finding: 1,
		Title:   "batch handler topK fallback",
		Symptom: "batch_handler.go propagated Diversity from the parent BatchRetrieveRequest but did not propagate TopK to its analytics emitter; sub-requests with TopK==0 wrote a misleading top_k=0 row even though the handler's internal default was applied. Fix: emit the analytics top_k from the effective sub-request TopK (default substituted when omitted).",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestBatch_AnalyticsTopKReportsDefaultWhenOmitted",
			Source:   "internal/retrieval/round11_review_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestBatch_AnalyticsTopKRespectsExplicitValue",
			Source:   "internal/retrieval/round11_review_test.go",
		}},
	},
	{
		PR:      "20",
		Finding: 2,
		Title:   "cache-warm analytics tagging",
		Symptom: "POST /v1/admin/retrieval/warm-cache wrote query_analytics rows with source=user, drowning real user traffic with warming noise. Fix: thread source=cache_warm through warm-cache, asserted by TestCacheWarmer_AnalyticsTaggedCacheWarmSource.",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestCacheWarmer_AnalyticsTaggedCacheWarmSource",
			Source:   "internal/retrieval/round11_review_test.go",
		}},
	},
	{
		PR:      "20",
		Finding: 3,
		Title:   "stream handler explain auth gate",
		Symptom: "POST /v1/retrieve/stream emitted explain traces (backend timings, score breakdown) regardless of the tenant's debug.explain ACL. Fix: gate explain emission on the same authorization check used by the non-streaming handler; only debug.explain holders see the trace.",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestStream_ExplainRequiresAuthorization",
			Source:   "internal/retrieval/round11_review_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestStream_ExplainEnabledByEnvFlag",
			Source:   "internal/retrieval/round11_review_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestStream_ExplainEnabledForAdminRole",
			Source:   "internal/retrieval/round11_review_test.go",
		}},
	},
	{
		PR:      "20",
		Finding: 4,
		Title:   "cache-aware backend_timings canonical schema",
		Symptom: "cache-hit responses projected backend timings with camelCase keys while cache-miss canonicalised via backendTimingsMillis() to snake_case source_*_ms; clients toggled between schemas based on cache state. Fix: project both branches through the same canonical mapping.",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestRetrieveWithSnapshotCached_RecordsBackendTimingsOnMiss",
			Source:   "internal/retrieval/round11_review_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestRetrieveWithSnapshotCached_RecordsBackendTimingsOnHit",
			Source:   "internal/retrieval/round11_review_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestGinHandler_CacheHit_RecordsBackendTimings",
			Source:   "internal/retrieval/round11_review_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestGinHandler_CacheMiss_RecordsBackendTimings",
			Source:   "internal/retrieval/round11_review_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestBackendTimingsSchemaParity",
			Source:   "internal/retrieval/backend_timings_schema_test.go",
		}},
	},
	{
		PR:      "20",
		Finding: 5,
		Title:   "hook panic recovery",
		Symptom: "runWithHookTimeout launched the hook goroutine without a defer-recover; a panic inside the user hook (e.g. nil-pointer in chunk_quality_recorder.go) would crash the pipeline pod. Fix: wrap the goroutine body in recover() and increment HookPanicsTotal{hook}.",
		Tests: []TestRef{{
			PkgRel:   "internal/pipeline",
			TestName: "TestRunWithHookTimeout_PanicInFnDoesNotCrashAndIncrementsPanicCounter",
			Source:   "internal/pipeline/hook_timeout_test.go",
		}, {
			PkgRel:   "internal/pipeline",
			TestName: "TestRunWithHookTimeout_TimeoutFiresOnSlowFn",
			Source:   "internal/pipeline/hook_timeout_test.go",
		}, {
			PkgRel:   "internal/pipeline",
			TestName: "TestRunWithHookTimeout_HappyPathReturnsFnError",
			Source:   "internal/pipeline/hook_timeout_test.go",
		}},
	},
	{
		PR:      "20",
		Finding: 6,
		Title:   "QueryAnalyticsSource constants pinned",
		Symptom: "internal/admin/query_analytics.go re-defined QueryAnalyticsSourceUser etc. on a per-call basis, which would silently allow typos like 'cache-warm' vs 'cache_warm'. Fix: pin the three exported constants (QueryAnalyticsSourceUser, _CacheWarm, _Batch) and reference everywhere; TestQueryAnalyticsRecorder_SourceFieldDefaults catches drift.",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestQueryAnalyticsRecorder_SourceFieldDefaults",
			Source:   "internal/admin/query_analytics_test.go",
		}},
	},
}
