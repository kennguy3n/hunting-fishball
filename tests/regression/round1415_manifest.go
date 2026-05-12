package regression

// round1415_manifest.go — Round-15 Task 12.
//
// Catalogues the regression fixes introduced during Round 14 (PR
// #24) and Round 15 (this PR). Round 15 expands the connector
// catalog from 12 to 20 entries and hardens every connector with
// explicit 429 → ErrRateLimited surfacing so the adaptive rate
// limiter in adaptive_rate.go can react.
//
// As with the earlier manifests, the meta-test in
// round1415_manifest_test.go asserts every named TestRef points
// at a real `func TestName(` symbol on disk.

// Round1415Manifest lists the Round-14 / Round-15 fixes pinned by
// dedicated regression tests.
var Round1415Manifest = []Bug{
	{
		PR:      "round-15",
		Finding: 1,
		Title:   "ErrRateLimited surfaced from every connector iterator",
		Symptom: "Round-14 connectors did not distinguish HTTP 429 from generic 5xx — the adaptive rate limiter in adaptive_rate.go could not react. Round-15 introduces the connector.ErrRateLimited sentinel; every iterator-fetchPage wraps 429 responses with it.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector",
			TestName: "TestConnectorAudit_Round15",
			Source:   "internal/connector/audit_test.go",
		}},
	},
	{
		PR:      "round-15",
		Finding: 2,
		Title:   "Google Shared Drives enumeration paginates beyond 100 drives",
		Symptom: "The Round-14 googledrive connector listed shared drives in a single `/drives?pageSize=100` request and dropped any tenant whose visible-drive set exceeded the page limit. Round-15 follows `nextPageToken` to completion.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/googledrive",
			TestName: "TestGoogleDrive_ListNamespaces_LargeSharedDriveSet",
			Source:   "internal/connector/googledrive/googledrive_test.go",
		}, {
			PkgRel:   "internal/connector/googledrive",
			TestName: "TestGoogleDrive_ListNamespaces_RateLimited",
			Source:   "internal/connector/googledrive/googledrive_test.go",
		}},
	},
	{
		PR:      "round-15",
		Finding: 3,
		Title:   "Connector registry expanded to 20 entries",
		Symptom: "PROPOSAL.md §4 listed KChat as a Phase-1 connector and Phase-2+ targets (S3, Linear, Asana, Discord, Salesforce, HubSpot, Google Shared Drives) — none registered. Round-15 wires all 8 into the registry; the smoke test pins the expected count.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestConnectorSmoke_RegistryHasAllConnectors",
			Source:   "tests/e2e/connector_smoke_test.go",
		}},
	},
	{
		PR:      "round-15",
		Finding: 4,
		Title:   "Slack rate-limit signalled via OK=false/error=ratelimited",
		Symptom: "Slack often signals throttling as an HTTP 200 response with `ok=false, error=ratelimited` rather than HTTP 429. The pre-Round-15 connector treated this as a generic failure. Round-15 wraps it with connector.ErrRateLimited.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector",
			TestName: "TestConnectorAudit_Round15",
			Source:   "internal/connector/audit_test.go",
		}},
	},
	{
		PR:      "round-15",
		Finding: 5,
		Title:   "KChat connector closes Phase-1 chat catalog",
		Symptom: "PROPOSAL.md §4 explicitly lists `Chat: Slack, KChat (internal)` as a Phase-1 connector pair. Slack landed but KChat did not — the catalog was missing the internal-tenant chat source until Round-15.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/kchat",
			TestName: "TestKChat_ListDocuments",
			Source:   "internal/connector/kchat/kchat_test.go",
		}, {
			PkgRel:   "internal/connector/kchat",
			TestName: "TestKChat_HandleWebhook_MessageEvent",
			Source:   "internal/connector/kchat/kchat_test.go",
		}},
	},
}
