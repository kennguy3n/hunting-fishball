package regression

// round1516_manifest.go — Round-16 Task 12.
//
// Catalogues the regression fixes introduced during Round 15 (PR
// #25) carried forward and the Round-16 catalog expansion. Round 16
// extends the connector catalog from 20 to 28 entries
// (mattermost, clickup, monday, pipedrive, okta, gmail, rss,
// confluence_server) and re-asserts every connector's DeltaSync
// bootstrap reflects "now" (DESC+limit=1) — not stale ascending
// queries — so adaptive_rate.go gets honest cursors on first call.
//
// As with the earlier manifests, the meta-test in
// round1516_manifest_test.go asserts every named TestRef points at
// a real `func TestName(` symbol on disk.

// Round1516Manifest lists the Round-15 / Round-16 fixes pinned by
// dedicated regression tests.
var Round1516Manifest = []Bug{
	{
		PR:      "round-16",
		Finding: 1,
		Title:   "Connector registry expanded to 28 entries (20 → 28)",
		Symptom: "PROPOSAL.md §4 Phase-2+ targets listed Chat/Mattermost, Issue/ClickUp+Monday, CRM/Pipedrive, Identity/Okta, Email/Gmail, Generic/RSS, and Knowledge/Confluence-Server — none registered. Round-16 wires all 8 into the registry; the smoke test pins the expected count.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestConnectorSmoke_RegistryHasAllConnectors",
			Source:   "tests/e2e/connector_smoke_test.go",
		}, {
			PkgRel:   "tests/e2e",
			TestName: "TestRound16_RegistryCount",
			Source:   "tests/e2e/round16_test.go",
		}},
	},
	{
		PR:      "round-16",
		Finding: 2,
		Title:   "DeltaSync initial cursor reflects 'now' for every Round-16 connector",
		Symptom: "Without a bootstrap cursor, a brand-new tenant subscribed to a fresh source would backfill the entire history on the first DeltaSync — defeating the cursor contract. Round-16 mirrors the Round-15 HubSpot/Linear/Salesforce/Asana fix: every new connector queries DESC + limit=1 to capture the upstream's current high-water mark without returning items.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/mattermost",
			TestName: "TestMattermost_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/mattermost/mattermost_test.go",
		}, {
			PkgRel:   "internal/connector/clickup",
			TestName: "TestClickUp_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/clickup/clickup_test.go",
		}, {
			PkgRel:   "internal/connector/monday",
			TestName: "TestMonday_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/monday/monday_test.go",
		}, {
			PkgRel:   "internal/connector/pipedrive",
			TestName: "TestPipedrive_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/pipedrive/pipedrive_test.go",
		}, {
			PkgRel:   "internal/connector/okta",
			TestName: "TestOkta_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/okta/okta_test.go",
		}, {
			PkgRel:   "internal/connector/gmail",
			TestName: "TestGmail_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/gmail/gmail_test.go",
		}, {
			PkgRel:   "internal/connector/rss",
			TestName: "TestRSS_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/rss/rss_test.go",
		}, {
			PkgRel:   "internal/connector/confluence_server",
			TestName: "TestConfluenceServer_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/confluence_server/confluence_server_test.go",
		}},
	},
	{
		PR:      "round-16",
		Finding: 3,
		Title:   "Monday.com GraphQL error envelope wraps to ErrRateLimited",
		Symptom: "Monday.com signals throttling as an HTTP 200 response carrying a GraphQL `errors[]` envelope with `extensions.code=ComplexityException` or `extensions.status_code=429`. Treating the response as a generic failure starved the adaptive rate limiter; Round-16 wraps the envelope with connector.ErrRateLimited.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/monday",
			TestName: "TestMonday_Connect_RateLimited_GraphQLEnvelope",
			Source:   "internal/connector/monday/monday_test.go",
		}},
	},
	{
		PR:      "round-16",
		Finding: 4,
		Title:   "Gmail history-cursor DeltaSync uses profile.historyId on bootstrap",
		Symptom: "Gmail's `history.list` endpoint requires a `startHistoryId`. On first call the connector has no cursor — Round-16 fetches `/users/me/profile.historyId` and returns it as the bootstrap cursor so subsequent calls receive a usable monotonic baseline.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/gmail",
			TestName: "TestGmail_DeltaSync_InitialCursorIsCurrent",
			Source:   "internal/connector/gmail/gmail_test.go",
		}, {
			PkgRel:   "tests/e2e",
			TestName: "TestRound16_GmailLifecycle",
			Source:   "tests/e2e/round16_test.go",
		}},
	},
	{
		PR:      "round-16",
		Finding: 5,
		Title:   "Okta deprovisioned status maps to ChangeDeleted",
		Symptom: "Okta does not delete records on deprovisioning — users move to status=DEPROVISIONED. Without translation, the downstream index would carry stale identities. Round-16 maps DEPROVISIONED to connector.ChangeDeleted in DeltaSync.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/okta",
			TestName: "TestOkta_DeltaSync_DeprovisionedDeleted",
			Source:   "internal/connector/okta/okta_test.go",
		}},
	},
	{
		PR:      "round-16",
		Finding: 6,
		Title:   "Audit test covers all 27 connectors (excluding google_shared_drives wrapper)",
		Symptom: "Round 15 introduced the connector audit gate but only the 19 catalog entries that existed then. Round-16 extends the gate to all 27 first-class connectors so every new connector must pass the ErrInvalidConfig/ErrNotSupported/ErrRateLimited/NewRequestWithContext checks.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector",
			TestName: "TestConnectorAudit_Round15",
			Source:   "internal/connector/audit_test.go",
		}},
	},
	{
		PR:      "round-16-review",
		Finding: 7,
		Title:   "Pipedrive api_token redacted from transport-level error strings",
		Symptom: "Pipedrive's REST v1 personal-token auth requires the api_token in the query string. Go's net/http embeds the full request URL in *url.Error.Error() on transport failures (EOF, connection refused, TLS errors), so the token would leak into structured logs via slog.Error in cmd/api and cmd/ingest. The do() helper now wraps every transport error through redactToken() which strips both the raw and url.QueryEscape'd forms.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/pipedrive",
			TestName: "TestPipedrive_NetworkErrorDoesNotLeakAPIToken",
			Source:   "internal/connector/pipedrive/pipedrive_test.go",
		}},
	},
	{
		PR:      "round-16-review",
		Finding: 8,
		Title:   "Confluence-Server DeltaSync CQL cursor preserves seconds precision",
		Symptom: "The CQL `lastModified > \"...\"` filter was formatted with `2006-01-02 15:04`, dropping seconds. Subsequent polls re-emitted every page updated in the trailing minute as duplicate ChangeUpserted events. Cursor advancement already uses full RFC3339 — only the outbound filter was lossy. Fixed by switching to `2006-01-02 15:04:05`.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/confluence_server",
			TestName: "TestConfluenceServer_DeltaSync_CursorPreservesSecondsPrecision",
			Source:   "internal/connector/confluence_server/confluence_server_test.go",
		}},
	},
	{
		PR:      "round-16-review",
		Finding: 9,
		Title:   "Gmail FetchDocument requests both Subject and From metadata headers",
		Symptom: "FetchDocument used url.Values.Set twice for the `metadataHeaders` key; the second call overwrote the first, so the outbound query only requested `From`. The Gmail API never returned the Subject header and doc.Title silently fell back to the snippet on every document. Switched the second call to url.Values.Add to allow repeated keys.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/gmail",
			TestName: "TestGmail_FetchDocument_RequestsBothSubjectAndFromHeaders",
			Source:   "internal/connector/gmail/gmail_test.go",
		}},
	},
	{
		PR:      "round-16-review",
		Finding: 10,
		Title:   "Pipedrive DeltaSync activities namespace emits changes (ies→y singular)",
		Symptom: "DeltaSync filtered /recents responses by `strings.TrimSuffix(ns.ID, \"s\")` to derive a singular item key. That gave \"activitie\" for the \"activities\" namespace, while Pipedrive returns item=\"activity\". Every activity change was silently dropped. Now strips \"ies\"→\"y\" before falling back to trimming \"s\", correctly handling deals/persons/activities.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/pipedrive",
			TestName: "TestPipedrive_DeltaSync_ActivitiesNamespaceEmitsChanges",
			Source:   "internal/connector/pipedrive/pipedrive_test.go",
		}},
	},
}
