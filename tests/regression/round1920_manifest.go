package regression

// round1920_manifest.go — Round-20/21 Task 11.
//
// Catalogues the regression fixes introduced during Round 19 and
// Round 20/21 (the PR that follows #28). Round 19 was the
// retrieval-cache + DLQ-analytics hardening series; Round 20/21
// adds the next eight connectors (zendesk, servicenow, freshdesk,
// airtable, trello, intercom, webex, bitbucket), lifting the
// catalog from 42 → 50, and ships a batch of production-cleanup
// fixes alongside the catalog expansion.
//
// As with the earlier manifests, the meta-test in
// round1920_manifest_test.go asserts every named TestRef points
// at a real `func TestName(` symbol on disk.

// Round1920Manifest lists the Round-19/20/21 fixes pinned by
// dedicated regression tests.
var Round1920Manifest = []Bug{
	{
		PR:      "round-20",
		Finding: 1,
		Title:   "Connector registry expanded to 50 entries (42 → 50)",
		Symptom: "PROPOSAL.md §4 Phase-7 listed Zendesk, ServiceNow, Freshdesk, Airtable, Trello, Intercom, Webex, and Bitbucket as the Round-20 catalog expansion — none were registered. Round-20 wires all 8 into the registry; the smoke test pins the expected count and the e2e suite asserts every Round-20 connector is present.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestConnectorSmoke_RegistryHasAllConnectors",
			Source:   "tests/e2e/connector_smoke_test.go",
		}, {
			PkgRel:   "tests/e2e",
			TestName: "TestRound20_RegistryCount",
			Source:   "tests/e2e/round20_test.go",
		}},
	},
	{
		PR:      "round-20",
		Finding: 2,
		Title:   "DeltaSync bootstrap returns 'now' for every Round-20 connector",
		Symptom: "Without a bootstrap cursor, a freshly-connected source would backfill the entire history on the first DeltaSync — defeating the cursor contract enshrined in Rounds 15/16/17/18. Round-20 mirrors the earlier rounds' fix: every new connector captures the upstream's current watermark (unix timestamp, RFC3339, or ServiceNow's '2006-01-02 15:04:05' format) without returning items.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/zendesk",
			TestName: "TestZendesk_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/zendesk/zendesk_test.go",
		}, {
			PkgRel:   "internal/connector/servicenow",
			TestName: "TestServiceNow_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/servicenow/servicenow_test.go",
		}, {
			PkgRel:   "internal/connector/freshdesk",
			TestName: "TestFreshdesk_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/freshdesk/freshdesk_test.go",
		}, {
			PkgRel:   "internal/connector/airtable",
			TestName: "TestAirtable_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/airtable/airtable_test.go",
		}, {
			PkgRel:   "internal/connector/trello",
			TestName: "TestTrello_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/trello/trello_test.go",
		}, {
			PkgRel:   "internal/connector/intercom",
			TestName: "TestIntercom_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/intercom/intercom_test.go",
		}, {
			PkgRel:   "internal/connector/webex",
			TestName: "TestWebex_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/webex/webex_test.go",
		}, {
			PkgRel:   "internal/connector/bitbucket",
			TestName: "TestBitbucket_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/bitbucket/bitbucket_test.go",
		}},
	},
	{
		PR:      "round-20",
		Finding: 3,
		Title:   "Rate-limit propagation for every Round-20 connector maps 429 → ErrRateLimited",
		Symptom: "Without ErrRateLimited propagation the auto-replay worker would treat an upstream-throttling 429 as a permanent failure and shelve the row in the DLQ. Round-20 wraps every Connect / DeltaSync 429 path on the eight new connectors with %w-formatted connector.ErrRateLimited so the existing categoriser routes them to the auto-replayable bucket, and the round20 e2e suite sweeps a 429 through Connect on each.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestRound20_RateLimitSweep",
			Source:   "tests/e2e/round20_test.go",
		}, {
			PkgRel:   "internal/connector/zendesk",
			TestName: "TestZendesk_Connect_RateLimited",
			Source:   "internal/connector/zendesk/zendesk_test.go",
		}, {
			PkgRel:   "internal/connector/servicenow",
			TestName: "TestServiceNow_Connect_RateLimited",
			Source:   "internal/connector/servicenow/servicenow_test.go",
		}, {
			PkgRel:   "internal/connector/freshdesk",
			TestName: "TestFreshdesk_Connect_RateLimited",
			Source:   "internal/connector/freshdesk/freshdesk_test.go",
		}, {
			PkgRel:   "internal/connector/airtable",
			TestName: "TestAirtable_Connect_RateLimited",
			Source:   "internal/connector/airtable/airtable_test.go",
		}, {
			PkgRel:   "internal/connector/trello",
			TestName: "TestTrello_Connect_RateLimited",
			Source:   "internal/connector/trello/trello_test.go",
		}, {
			PkgRel:   "internal/connector/intercom",
			TestName: "TestIntercom_Connect_RateLimited",
			Source:   "internal/connector/intercom/intercom_test.go",
		}, {
			PkgRel:   "internal/connector/webex",
			TestName: "TestWebex_Connect_RateLimited",
			Source:   "internal/connector/webex/webex_test.go",
		}, {
			PkgRel:   "internal/connector/bitbucket",
			TestName: "TestBitbucket_Connect_RateLimited",
			Source:   "internal/connector/bitbucket/bitbucket_test.go",
		}},
	},
	{
		PR:      "round-20",
		Finding: 4,
		Title:   "Heterogeneous Round-20 lifecycles (Zendesk + Bitbucket) survive the full Validate → FetchDocument → DeltaSync walk",
		Symptom: "A connector that registered but had a broken FetchDocument or namespace iterator would silently drop documents at Stage 1. Round-20 ships dedicated e2e lifecycle tests for two heterogeneous Round-20 surfaces (Zendesk's incremental-export REST API and Bitbucket's pull-request query) so a regression on either is caught at the e2e gate.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestRound20_ZendeskLifecycle",
			Source:   "tests/e2e/round20_test.go",
		}, {
			PkgRel:   "tests/e2e",
			TestName: "TestRound20_BitbucketLifecycle",
			Source:   "tests/e2e/round20_test.go",
		}},
	},
	{
		PR:      "round-20",
		Finding: 5,
		Title:   "Connector runbooks pinned for every Round-20 entry",
		Symptom: "An undocumented connector landing in production is an on-call hazard — the next page would be answered with no rotation, quota, or outage-detection guidance. Round-20 adds docs/runbooks/{zendesk,servicenow,freshdesk,airtable,trello,intercom,webex,bitbucket}.md and lifts the runbook_test floor from 42 → 50 so a missing runbook fails CI.",
		Tests: []TestRef{{
			PkgRel:   "docs/runbooks",
			TestName: "TestConnectorRunbooks_ExistAndCoverRequiredSections",
			Source:   "docs/runbooks/runbook_test.go",
		}},
	},
	{
		PR:      "round-20",
		Finding: 6,
		Title:   "Connector contract assertion + empty-cursor table-driven test extended to Round-20 connectors",
		Symptom: "The integration contract test only enforced that every Round-15/16/17/18 connector implemented SourceConnector + DeltaSyncer at compile time; the Round-20 additions needed the same gate. Round-20 extends the compile-time assertion set to cover the eight new structs and the empty-cursor table-driven test now spans Zendesk's incremental-export, ServiceNow's sys_updated_on, and Bitbucket's q= query surfaces.",
		Tests: []TestRef{{
			PkgRel:   "tests/integration",
			TestName: "TestConnectorContract_Round20_DeltaSyncerEmptyCursor",
			Source:   "tests/integration/connector_contract_test.go",
		}},
	},
}
