// Package regression catalogues regression tests by originating
// bug. Each entry maps a Devin Review finding (or other inbound
// bug report) to the test(s) that pin its fix.
//
// Round-4 Task 15: when a reviewer opens a regression test in
// isolation it is not obvious why it exists. The manifest is the
// glue: every Bug entry below names the originating PR, the
// finding number, the file paths of the regression tests, and a
// one-line explanation of what the regression looks like in the
// wild. The TestManifestCovers... test below executes the manifest
// to confirm every named test is still present in the source tree.
package regression

// Bug is one originating bug + its regression coverage.
type Bug struct {
	// PR that surfaced the bug, e.g. "12".
	PR string

	// Finding (1-indexed) on that PR's review thread, used to
	// disambiguate when one PR collected several bugs in one
	// review.
	Finding int

	// Title is a one-line description of the bug.
	Title string

	// Symptom is what the user saw before the fix landed.
	Symptom string

	// Tests is the list of (package import path, test name)
	// regression tests that pin the fix. Multiple tests are
	// allowed when the same fix needs orthogonal coverage.
	Tests []TestRef
}

// TestRef names one regression test.
type TestRef struct {
	// PkgRel is the package path relative to the repo root.
	PkgRel string

	// TestName is the Go test function name.
	TestName string

	// Source is the file the test lives in, relative to the
	// repo root, used for cross-referencing in code review.
	Source string
}

// Manifest is the canonical list of regressions. Append-only —
// removing an entry should require explicit reviewer sign-off.
var Manifest = []Bug{
	{
		PR:      "12",
		Finding: 1,
		Title:   "audit search LIKE injection",
		Symptom: "client passing q=% returned every row, leaking other tenants' audit metadata",
		Tests: []TestRef{
			{
				PkgRel:   "internal/audit",
				TestName: "TestRepository_ListPayloadSearchEscapesLikeWildcards",
				Source:   "internal/audit/repository_test.go",
			},
		},
	},
	{
		PR:      "12",
		Finding: 2,
		Title:   "ingest graceful shutdown deadlock",
		Symptom: "SIGTERM during a draining batch hung indefinitely on the in-flight channel",
		Tests: []TestRef{
			{
				PkgRel:   "cmd/ingest",
				TestName: "TestWaitChanClosed_DrainedChannelDoesNotBlock",
				Source:   "cmd/ingest/main_test.go",
			},
		},
	},
	{
		PR:      "12",
		Finding: 3,
		Title:   "batch retrieve cache bypass",
		Symptom: "fanned-out batch queries skipped the semantic cache; every call hit Qdrant",
		Tests: []TestRef{
			{
				PkgRel:   "internal/retrieval",
				TestName: "TestBatch_CacheHitsServeWithoutFanOut",
				Source:   "internal/retrieval/batch_handler_test.go",
			},
		},
	},
	{
		PR:      "12",
		Finding: 4,
		Title:   "sync_progress upsert race",
		Symptom: "two concurrent first-inserts each created a row; subsequent updates collided on PK",
		Tests: []TestRef{
			{
				PkgRel:   "internal/admin",
				TestName: "TestSyncProgress_ConcurrentFirstInsert",
				Source:   "internal/admin/sync_progress_test.go",
			},
			{
				PkgRel:   "internal/admin",
				TestName: "TestSyncProgress_ConcurrentIncrement",
				Source:   "internal/admin/sync_progress_test.go",
			},
		},
	},
	{
		PR:      "12",
		Finding: 5,
		Title:   "DLQ MarkReplayed ordering",
		Symptom: "replay handler re-enqueued the same message twice when the offset bump failed mid-flight",
		Tests: []TestRef{
			{
				PkgRel:   "internal/pipeline",
				TestName: "TestReplayer_Replay_BumpFailureStillMarksReplayed",
				Source:   "internal/pipeline/dlq_consumer_test.go",
			},
			{
				PkgRel:   "internal/pipeline",
				TestName: "TestReplayer_Replay_ProducerFailureMarksReplayedWithError",
				Source:   "internal/pipeline/dlq_consumer_test.go",
			},
		},
	},
	{
		PR:      "12",
		Finding: 6,
		Title:   "DLQ list pagination phantom token",
		Symptom: "list endpoint returned a next_page_token even when the page filled exactly, sending the client to an empty page",
		Tests: []TestRef{
			{
				PkgRel:   "internal/admin",
				TestName: "TestDLQHandler_List_PaginationBoundaries",
				Source:   "internal/admin/dlq_handler_test.go",
			},
		},
	},
}
