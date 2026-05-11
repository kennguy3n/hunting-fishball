package regression

// round78_manifest.go — Round-9 Task 15.
//
// Round-9 catalogues the Devin Review findings that landed during
// Round 7 (PR #16) and Round 8 (PR #17). Each entry below names
// the originating commit SHA + the regression test pinning the
// fix. The existing Manifest (tests/regression/manifest.go) covers
// the original Round-4 bugs; Round78Manifest extends it with the
// later Devin Review iterations so a future reviewer can answer
// "why does this test exist?" without spelunking the git log.
//
// Append-only — see Manifest's docstring for the policy.

// Round78Manifest lists the Devin Review findings from PRs #16 and
// #17, in commit order. The TestManifestRound78Refs... meta-test
// confirms every named test/file exists in the tree.
var Round78Manifest = []Bug{
	// ----- PR #16 (Round 7) Devin Review fixes --------------------

	{
		PR:      "16",
		Finding: 1,
		Title:   "pin_apply sparse positions",
		Symptom: "applying a single pin at position 5 to a 2-result hit list silently dropped the pin instead of padding/inserting at the requested slot",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestApplyPins_SparsePositions",
			Source:   "internal/retrieval/pin_apply_test.go",
		}},
	},
	{
		PR:      "16",
		Finding: 2,
		Title:   "latency-budget cardinality",
		Symptom: "budget-violation metric was labelled with tenant_id, threatening unbounded Prometheus cardinality on a multi-tenant cluster",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestLatencyBudgetHandler_GetPut",
			Source:   "internal/admin/latency_budget_test.go",
		}},
	},
	{
		PR:      "16",
		Finding: 3,
		Title:   "cache_warm error handling",
		Symptom: "TopQueries error from analytics fell through silently, causing the auto-warm path to warm nothing without surfacing the failure to operators",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestCacheWarmHandler_AutoFromAnalytics",
			Source:   "internal/admin/cache_warm_handler_test.go",
		}},
	},
	{
		PR:      "16",
		Finding: 4,
		Title:   "cache_config / sync_history / ABTestAggregator",
		Symptom: "follow-on Devin Review surfacing additional Round-7 polish (cache_config row schema, sync_history pagination, ABTestAggregator p-value sanity)",
		Tests: []TestRef{
			{
				PkgRel:   "internal/admin",
				TestName: "TestCacheConfigHandler_PutGet",
				Source:   "internal/admin/cache_config_test.go",
			},
			{
				PkgRel:   "internal/admin",
				TestName: "TestSyncHistory_Handler",
				Source:   "internal/admin/sync_history_test.go",
			},
		},
	},

	// ----- PR #17 (Round 8) Devin Review fixes --------------------

	{
		PR:      "17",
		Finding: 1,
		Title:   "429 retry handling",
		Symptom: "isRetryableResponseCode treated 429 as permanent, contradicting WebhookDelivery.Send which already retried it — so a 429 inside the dispatcher was both retried and dead-lettered, resulting in dropped notifications under throttling",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestIsRetryableResponseCode",
			Source:   "internal/admin/notification_retryable_test.go",
		}},
	},
	{
		PR:      "17",
		Finding: 2,
		Title:   "retry worker attempt counter",
		Symptom: "the retry worker added DeliveryResult.Attempts (inner-HTTP retries) onto the row's Attempt counter every Tick, so a single flaky 5xx endpoint would push the row past MaxAttempts after one cycle and dead-letter prematurely",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestNotificationRetryWorker_AttemptIncrementsByWorkerCycle",
			Source:   "internal/admin/notification_retry_worker_test.go",
		}},
	},
	{
		PR:      "17",
		Finding: 3,
		Title:   "phantom-notification on rollback",
		Symptom: "NotifyingAuditWriter.CreateInTx fired the dispatcher immediately after the audit row was written into the tx — if the caller then rolled back the tx, subscribers received a notification for an audit event that no longer exists in the database",
		Tests: []TestRef{
			{
				PkgRel:   "internal/admin",
				TestName: "TestNotifyingAuditWriter_CreateInTx_RolledBackTxNoDispatch",
				Source:   "internal/admin/notification_audit_test.go",
			},
			{
				PkgRel:   "internal/admin",
				TestName: "TestNotifyingAuditWriter_CreateInTx_FlushAfterCommitDispatches",
				Source:   "internal/admin/notification_audit_test.go",
			},
		},
	},
}
