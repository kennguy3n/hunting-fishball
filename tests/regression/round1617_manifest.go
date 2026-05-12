package regression

// round1617_manifest.go — Round-17 Task 12.
//
// Catalogues the regression fixes introduced during Round 17 (PR
// #27 — connector catalog expansion from 28 → 36). Round 17 adds
// entra_id, google_workspace, outlook, workday, bamboohr,
// personio, sitemap, and coda. Each connector follows the
// Round-15/16 DeltaSync bootstrap contract: empty cursor on first
// call returns a "now" timestamp (or upstream delta-token /
// last-mod), never a historical backfill.
//
// As with the earlier manifests, the meta-test in
// round1617_manifest_test.go asserts every named TestRef points
// at a real `func TestName(` symbol on disk.

// Round1617Manifest lists the Round-17 fixes pinned by dedicated
// regression tests.
var Round1617Manifest = []Bug{
	{
		PR:      "round-17",
		Finding: 1,
		Title:   "Connector registry expanded to 36 entries (28 → 36)",
		Symptom: "PROPOSAL.md §4 Phase-2+ targets listed Identity/Entra-ID, Identity/Google-Workspace, Email/Outlook, HR/Workday, HR/BambooHR, HR/Personio, Generic/Sitemap, and Knowledge/Coda — none registered. Round-17 wires all 8 into the registry; the smoke test pins the expected count.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestConnectorSmoke_RegistryHasAllConnectors",
			Source:   "tests/e2e/connector_smoke_test.go",
		}, {
			PkgRel:   "tests/e2e",
			TestName: "TestRound17_RegistryCount",
			Source:   "tests/e2e/round17_test.go",
		}},
	},
	{
		PR:      "round-17",
		Finding: 2,
		Title:   "DeltaSync bootstrap returns 'now' for every Round-17 connector",
		Symptom: "Without a bootstrap cursor, a brand-new tenant subscribed to a fresh source would backfill the entire history on the first DeltaSync — defeating the cursor contract. Round-17 mirrors the Round-15/16 fix: every new connector queries DESC + limit=1 (or fetches the upstream delta token / lastmod) to capture the upstream's current high-water mark without returning items.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/entra_id",
			TestName: "TestEntraID_DeltaSync_BootstrapReturnsDeltaLink",
			Source:   "internal/connector/entra_id/entra_id_test.go",
		}, {
			PkgRel:   "internal/connector/google_workspace",
			TestName: "TestGoogleWorkspace_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/google_workspace/google_workspace_test.go",
		}, {
			PkgRel:   "internal/connector/outlook",
			TestName: "TestOutlook_DeltaSync_BootstrapReturnsDeltaLink",
			Source:   "internal/connector/outlook/outlook_test.go",
		}, {
			PkgRel:   "internal/connector/workday",
			TestName: "TestWorkday_DeltaSync_BootstrapReturnsLatestLastModified",
			Source:   "internal/connector/workday/workday_test.go",
		}, {
			PkgRel:   "internal/connector/bamboohr",
			TestName: "TestBambooHR_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/bamboohr/bamboohr_test.go",
		}, {
			PkgRel:   "internal/connector/personio",
			TestName: "TestPersonio_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/personio/personio_test.go",
		}, {
			PkgRel:   "internal/connector/sitemap",
			TestName: "TestSitemap_DeltaSync_BootstrapAndIncrement",
			Source:   "internal/connector/sitemap/sitemap_test.go",
		}, {
			PkgRel:   "internal/connector/coda",
			TestName: "TestCoda_DeltaSync_BootstrapReturnsLatestUpdatedAt",
			Source:   "internal/connector/coda/coda_test.go",
		}},
	},
	{
		PR:      "round-17",
		Finding: 3,
		Title:   "Identity connectors map deprovisioned principals to ChangeDeleted",
		Symptom: "Identity providers signal removal via deprovisioned / disabled / suspended flags rather than HTTP-404. Round-17's Entra ID, Google Workspace, Workday, and Personio connectors translate those upstream signals into connector.ChangeDeleted so downstream graph rebuilds see consistent tombstones.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/entra_id",
			TestName: "TestEntraID_DeltaSync_RemovedAndDisabledMappedToDeleted",
			Source:   "internal/connector/entra_id/entra_id_test.go",
		}, {
			PkgRel:   "internal/connector/google_workspace",
			TestName: "TestGoogleWorkspace_DeltaSync_SuspendedMappedToDeleted",
			Source:   "internal/connector/google_workspace/google_workspace_test.go",
		}, {
			PkgRel:   "internal/connector/workday",
			TestName: "TestWorkday_DeltaSync_TerminatedMappedToDeleted",
			Source:   "internal/connector/workday/workday_test.go",
		}, {
			PkgRel:   "internal/connector/personio",
			TestName: "TestPersonio_DeltaSync_InactiveMappedToDeleted",
			Source:   "internal/connector/personio/personio_test.go",
		}},
	},
	{
		PR:      "round-17",
		Finding: 4,
		Title:   "All Round-17 connectors wrap HTTP 429 with ErrRateLimited",
		Symptom: "Treating an upstream 429 as a generic error starves the adaptive rate limiter in adaptive_rate.go. Round-17 keeps the Round-15 contract: every new connector's Connect / ListDocuments / DeltaSync paths surface connector.ErrRateLimited on 429.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestRound17_RateLimitSweep",
			Source:   "tests/e2e/round17_test.go",
		}, {
			PkgRel:   "internal/connector/entra_id",
			TestName: "TestEntraID_DeltaSync_RateLimited",
			Source:   "internal/connector/entra_id/entra_id_test.go",
		}, {
			PkgRel:   "internal/connector/google_workspace",
			TestName: "TestGoogleWorkspace_DeltaSync_RateLimited",
			Source:   "internal/connector/google_workspace/google_workspace_test.go",
		}, {
			PkgRel:   "internal/connector/outlook",
			TestName: "TestOutlook_DeltaSync_RateLimited",
			Source:   "internal/connector/outlook/outlook_test.go",
		}, {
			PkgRel:   "internal/connector/workday",
			TestName: "TestWorkday_DeltaSync_RateLimited",
			Source:   "internal/connector/workday/workday_test.go",
		}, {
			PkgRel:   "internal/connector/bamboohr",
			TestName: "TestBambooHR_DeltaSync_RateLimited",
			Source:   "internal/connector/bamboohr/bamboohr_test.go",
		}, {
			PkgRel:   "internal/connector/personio",
			TestName: "TestPersonio_DeltaSync_RateLimited",
			Source:   "internal/connector/personio/personio_test.go",
		}, {
			PkgRel:   "internal/connector/sitemap",
			TestName: "TestSitemap_DeltaSync_RateLimited",
			Source:   "internal/connector/sitemap/sitemap_test.go",
		}, {
			PkgRel:   "internal/connector/coda",
			TestName: "TestCoda_DeltaSync_RateLimited",
			Source:   "internal/connector/coda/coda_test.go",
		}},
	},
	{
		PR:      "round-17",
		Finding: 5,
		Title:   "BambooHR basic-auth header sends api_key as user, 'x' as password",
		Symptom: "BambooHR's REST API expects HTTP basic auth where the api_key is the username and a literal 'x' is the password (so a typo'd key produces a 401 instead of leaking the key into a query string). The Round-17 connector pins this contract; a regression that flipped the parameters would silently re-introduce the leak vector.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/bamboohr",
			TestName: "TestBambooHR_BasicAuthHeader",
			Source:   "internal/connector/bamboohr/bamboohr_test.go",
		}},
	},
	{
		PR:      "round-17",
		Finding: 6,
		Title:   "Sitemap connector recurses sitemap-index → child sitemaps",
		Symptom: "Large sites publish a sitemap-index that points at multiple child sitemap.xml shards. A naive parser that only reads <urlset> would miss every entry. Round-17 expands <sitemapindex> entries by fetching each child sitemap, capped by maxDepth to avoid cycles.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/sitemap",
			TestName: "TestSitemap_ListDocuments_BasicAndIndex",
			Source:   "internal/connector/sitemap/sitemap_test.go",
		}},
	},
}
