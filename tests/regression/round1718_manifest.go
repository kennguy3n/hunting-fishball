package regression

// round1718_manifest.go — Round-18 Task 16.
//
// Catalogues the regression fixes introduced during Round 18 (PR
// #28 — connector catalog expansion from 36 → 42 plus production
// hardening). Round 18 adds sharepoint_onprem, azure_blob, gcs,
// egnyte, bookstack, and upload_portal. Each new connector
// follows the Round-15/16/17 DeltaSync bootstrap contract: empty
// cursor on first call returns a "now" timestamp / upstream
// delta-token / last-modified watermark — never a historical
// backfill.
//
// As with the earlier manifests, the meta-test in
// round1718_manifest_test.go asserts every named TestRef points
// at a real `func TestName(` symbol on disk.

// Round1718Manifest lists the Round-18 fixes pinned by dedicated
// regression tests.
var Round1718Manifest = []Bug{
	{
		PR:      "round-18",
		Finding: 1,
		Title:   "Connector registry expanded to 42 entries (36 → 42)",
		Symptom: "PROPOSAL.md §4 Phase-2+ targets listed SharePoint-Server, Azure-Blob, GCS, Egnyte, BookStack, and the signed-upload portal — none registered. Round-18 wires all 6 into the registry; the smoke test pins the expected count.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestConnectorSmoke_RegistryHasAllConnectors",
			Source:   "tests/e2e/connector_smoke_test.go",
		}, {
			PkgRel:   "tests/e2e",
			TestName: "TestRound18_RegistryCount",
			Source:   "tests/e2e/round18_test.go",
		}},
	},
	{
		PR:      "round-18",
		Finding: 2,
		Title:   "DeltaSync bootstrap returns 'now' / delta-token for every Round-18 connector",
		Symptom: "Without a bootstrap cursor, a freshly-connected source would backfill the entire history on the first DeltaSync — defeating the cursor contract. Round-18 mirrors the earlier rounds' fix: every new connector queries DESC + limit=1 (or fetches the upstream delta token / lastmod) to capture the upstream's current high-water mark without returning items.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/sharepoint_onprem",
			TestName: "TestSharePointOnprem_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/sharepoint_onprem/sharepoint_onprem_test.go",
		}, {
			PkgRel:   "internal/connector/azure_blob",
			TestName: "TestAzureBlob_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/azure_blob/azure_blob_test.go",
		}, {
			PkgRel:   "internal/connector/gcs",
			TestName: "TestGCS_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/gcs/gcs_test.go",
		}, {
			PkgRel:   "internal/connector/egnyte",
			TestName: "TestEgnyte_DeltaSync_BootstrapReturnsCursor",
			Source:   "internal/connector/egnyte/egnyte_test.go",
		}, {
			PkgRel:   "internal/connector/bookstack",
			TestName: "TestBookstack_DeltaSync_BootstrapReturnsLatest",
			Source:   "internal/connector/bookstack/bookstack_test.go",
		}},
	},
	{
		PR:      "round-18",
		Finding: 3,
		Title:   "Rate-limit propagation for all Round-18 connectors maps 429 → ErrRateLimited",
		Symptom: "Without ErrRateLimited propagation the auto-replay worker would treat an upstream-throttling 429 as a permanent failure and shelve the row in the DLQ. Round-18 wraps every Connect / DeltaSync 429 path with %w-formatted connector.ErrRateLimited so the existing categoriser routes them to the auto-replayable bucket.",
		Tests: []TestRef{{
			PkgRel:   "tests/e2e",
			TestName: "TestRound18_RateLimitSweep",
			Source:   "tests/e2e/round18_test.go",
		}, {
			PkgRel:   "internal/connector/sharepoint_onprem",
			TestName: "TestSharePointOnprem_Connect_RateLimited",
			Source:   "internal/connector/sharepoint_onprem/sharepoint_onprem_test.go",
		}, {
			PkgRel:   "internal/connector/azure_blob",
			TestName: "TestAzureBlob_Connect_RateLimited",
			Source:   "internal/connector/azure_blob/azure_blob_test.go",
		}, {
			PkgRel:   "internal/connector/gcs",
			TestName: "TestGCS_Connect_RateLimited",
			Source:   "internal/connector/gcs/gcs_test.go",
		}, {
			PkgRel:   "internal/connector/egnyte",
			TestName: "TestEgnyte_Connect_RateLimited",
			Source:   "internal/connector/egnyte/egnyte_test.go",
		}, {
			PkgRel:   "internal/connector/bookstack",
			TestName: "TestBookstack_Connect_RateLimited",
			Source:   "internal/connector/bookstack/bookstack_test.go",
		}},
	},
	{
		PR:      "round-18",
		Finding: 4,
		Title:   "Upload-portal webhook rejects oversize / bad-MIME / bad-signature uploads",
		Symptom: "A signed-upload portal that trusted user-controlled file metadata would be a denial-of-service vector and a tenant-isolation hole. Round-18's upload_portal connector validates HMAC-SHA256 signature, content-type allow-list, and size cap before emitting the SourceDocument.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/upload_portal",
			TestName: "TestUploadPortal_HandleWebhookFor_RejectsBadMIME",
			Source:   "internal/connector/upload_portal/upload_portal_test.go",
		}, {
			PkgRel:   "internal/connector/upload_portal",
			TestName: "TestUploadPortal_HandleWebhookFor_RejectsOversize",
			Source:   "internal/connector/upload_portal/upload_portal_test.go",
		}, {
			PkgRel:   "internal/connector/upload_portal",
			TestName: "TestUploadPortal_HandleWebhookFor_RejectsBadSignature",
			Source:   "internal/connector/upload_portal/upload_portal_test.go",
		}},
	},
	{
		PR:      "round-18",
		Finding: 5,
		Title:   "Cross-encoder reranker falls back gracefully on sidecar outage",
		Symptom: "A reranker sidecar that returned an RPC error would otherwise demote every hit to score=0, destroying retrieval quality. Round-18 introduces a CrossEncoderReranker that delegates to the in-process LinearReranker fallback on every transport failure so a Python sidecar outage degrades to the baseline rather than dropping results.",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestCrossEncoderReranker_FallbackOnRPCError",
			Source:   "internal/retrieval/cross_encoder_reranker_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestCrossEncoderReranker_NilClientUsesFallback",
			Source:   "internal/retrieval/cross_encoder_reranker_test.go",
		}},
	},
	{
		PR:      "round-18",
		Finding: 6,
		Title:   "Embedding model versioning marks chunks stale on model rotation",
		Symptom: "When a tenant rotated their embedding model via PUT /v1/admin/sources/:id/embedding, existing Qdrant vectors were silently stale — query embeddings landed in a different space. Round-18 introduces the chunk_embedding_version table and a StaleEmbeddingDetector that stamps stale_since on every divergent row so the worker can re-embed them through Stage 3.",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestStaleEmbeddingDetector_MarksDivergentRows",
			Source:   "internal/admin/stale_embedding_test.go",
		}, {
			PkgRel:   "internal/admin",
			TestName: "TestStaleEmbeddingWorker_DrainsRows",
			Source:   "internal/admin/stale_embedding_test.go",
		}, {
			PkgRel:   "internal/admin",
			TestName: "TestStaleEmbeddingWorker_RecordsFailure",
			Source:   "internal/admin/stale_embedding_test.go",
		}},
	},
	{
		PR:      "round-18",
		Finding: 7,
		Title:   "Tenant onboarding wizard rejects cross-tenant access",
		Symptom: "Without an explicit tenant-id mismatch guard, a holder of tenant-a's JWT could probe tenant-b's onboarding status. Round-18 introduces POST /v1/admin/tenants/:tenant_id/onboarding which 403s when the path tenant id differs from the JWT-derived tenant id.",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestOnboarding_RejectsTenantMismatch",
			Source:   "internal/admin/onboarding_handler_test.go",
		}, {
			PkgRel:   "internal/admin",
			TestName: "TestOnboarding_RejectsMissingContext",
			Source:   "internal/admin/onboarding_handler_test.go",
		}},
	},
	{
		PR:      "round-18",
		Finding: 8,
		Title:   "Per-chunk explain breakdown surfaces freshness/pin/MMR/cross-encoder/bm25-terms",
		Symptom: "Round-13 added a coarse-grained explain block; operators triaging unexpected rankings could see only the top-line RRF score and reranker delta. Round-18 extends RetrievalExplain to expose the freshness boost, pin boost, MMR diversity penalty, cross-encoder score, and per-term BM25 contributions when the adapters report them.",
		Tests: []TestRef{{
			PkgRel:   "internal/retrieval",
			TestName: "TestBuildExplain_PopulatesRound18Round14Fields",
			Source:   "internal/retrieval/explain_test.go",
		}, {
			PkgRel:   "internal/retrieval",
			TestName: "TestBuildExplain_OmitsRound18Round14FieldsWhenMetadataMissing",
			Source:   "internal/retrieval/explain_test.go",
		}},
	},
}
