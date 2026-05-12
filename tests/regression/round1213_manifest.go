package regression

// round1213_manifest.go — Round-14 Task 9.
//
// Catalogues the Devin Review fixes that landed during Round-13
// (PR #22). The manifest follows the same append-only contract
// as the earlier round manifests: a meta-test in
// round1213_manifest_test.go asserts every named regression
// test exists in the tree.
//
// Round 13 introduced large new subsystems (stage circuit
// breakers, SLO burn-rate alerts, API-key rotation with grace
// periods, audit-log integrity verification, degraded-mode
// embedding fallback, per-request payload size limiting). Each
// entry below names the originating bug or design contract that
// the regression test pins.

// Round1213Manifest lists the Round-13 / PR #22 review-driven
// regression coverage.
var Round1213Manifest = []Bug{
	{
		PR:      "22",
		Finding: 1,
		Title:   "stage breaker probe gate",
		Symptom: "Round-13 Task 5 introduced StageCircuitBreaker but the half-open `probeInFlight` flag was not released on early-exit paths; once the breaker tripped half-open a single failed probe could lock the gate forever. Fix: release probeInFlight on OnSuccess, OnFailure, and the early-exit / context-cancellation paths.",
		Tests: []TestRef{{
			PkgRel:   "internal/pipeline",
			TestName: "TestStageBreaker_HalfOpenAllowsSingleProbe",
			Source:   "internal/pipeline/stage_breaker_test.go",
		}, {
			PkgRel:   "internal/pipeline",
			TestName: "TestStageBreaker_HalfOpenProbeFailureReleasesGate",
			Source:   "internal/pipeline/stage_breaker_test.go",
		}},
	},
	{
		PR:      "22",
		Finding: 2,
		Title:   "SLO burn-rate 1h+5m multi-window",
		Symptom: "Round-13 Task 2 burn-rate rule originally fired on any 5m window over the SLO, producing noisy paging during normal transient spikes. Fix: require BOTH the 1h trend AND the 5m window to exceed the burn budget before firing.",
		Tests: []TestRef{{
			PkgRel:   "deploy",
			TestName: "TestAlertsManifest_Round13BurnRate",
			Source:   "deploy/alerts_test.go",
		}},
	},
	{
		PR:      "22",
		Finding: 3,
		Title:   "BackendContributions populated in runPipelineFromVec",
		Symptom: "Round-13 Task 7 added BackendContributions to the explain projection but runPipelineFromVec dropped the field on cache-miss with snapshot+vec inputs, producing empty contribution arrays despite real backend involvement. Fix: thread the merge-side contribution map through the runPipelineFromVec path.",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestExplain_BackendContributions_Round13",
			Source:   "internal/retrieval/explain_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestExplain_BackendContributions_OmittedWhenNotExplain",
			Source:   "internal/retrieval/explain_test.go",
		}},
	},
	{
		PR:      "22",
		Finding: 4,
		Title:   "api key rotation atomic Rotate",
		Symptom: "Round-13 Task 10 first version rotated keys via two separate UPDATEs (flip-old + insert-new) outside a transaction; a crash between calls left the tenant with two active rows. Fix: rotate inside a single tx so the flip-old + insert-new commit atomically.",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestAPIKeyStoreGORM_Rotate_HappyPath",
			Source:   "internal/admin/api_key_rotation_test.go",
		}, {
			PkgRel:   "internal/admin",
			TestName: "TestAPIKeyStoreGORM_Rotate_RollsBackOnInsertFailure",
			Source:   "internal/admin/api_key_rotation_test.go",
		}},
	},
	{
		PR:      "22",
		Finding: 5,
		Title:   "stage breaker early-exit path probe release",
		Symptom: "When Allow() arms the probe and the caller short-circuits (e.g. context canceled before the stage runs), the gate stayed armed because neither OnSuccess nor OnFailure was called. Fix: surface a Release() path that callers invoke from defer to unconditionally drop the gate.",
		Tests: []TestRef{{
			PkgRel:   "internal/pipeline",
			TestName: "TestStageBreaker_HalfOpenProbeFailureReleasesGate",
			Source:   "internal/pipeline/stage_breaker_test.go",
		}},
	},
	{
		PR:      "22",
		Finding: 6,
		Title:   "batch trace ID echo",
		Symptom: "Round-13 Task 3 added a parent span over /v1/retrieve/batch but the trace_id was not surfaced in BatchResponse, so operators could not correlate the parent and the N children in Jaeger. Fix: echo the trace id back to the caller.",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestBatch_TraceIDEcho_Round13Task3",
			Source:   "internal/retrieval/batch_handler_test.go",
		}},
	},
}
