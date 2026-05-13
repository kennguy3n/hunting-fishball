package regression

// round2223_manifest.go — Round-24 Task 8.
//
// Catalogues the regression fixes pinned by dedicated tests
// during Round 22 and Round 23. Round 22 was the Devin-Review
// follow-up to the Round-20/21 catalog expansion (Airtable
// cursor parsing, Freshdesk Link-header pagination, Qdrant
// readiness probes, ConnectorHealthHandler wiring); Round 23
// shipped the Intercom namespace branching fix, the
// paused-source filter on the worker fan-out, and the
// connector_sync_cursors migration that backstops
// per-connector cursor tracking.
//
// As with the earlier manifests, the meta-test in
// round2223_manifest_test.go asserts every named TestRef
// points at a real `func TestName(` symbol on disk.

// Round2223Manifest lists the Round-22/23 fixes pinned by
// dedicated regression tests.
var Round2223Manifest = []Bug{
	{
		PR:      "round-22",
		Finding: 1,
		Title:   "Airtable DeltaSync cursor parses both seconds and microseconds since epoch",
		Symptom: "Round-21 wrote the Airtable cursor as a unix-seconds string but the parser only accepted RFC3339 — first DeltaSync after upgrade rebackfilled the entire base. Round-22 makes the cursor parser accept both forms and the bootstrap path emit the canonical RFC3339 layout.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/airtable",
			TestName: "TestAirtable_DeltaSync_BootstrapReturnsNow",
			Source:   "internal/connector/airtable/airtable_test.go",
		}, {
			PkgRel:   "internal/connector/airtable",
			TestName: "TestAirtable_DeltaSync_Incremental",
			Source:   "internal/connector/airtable/airtable_test.go",
		}},
	},
	{
		PR:      "round-22",
		Finding: 2,
		Title:   "Freshdesk pagination walks Link rel=\"next\" beyond first page",
		Symptom: "The Round-20 implementation stopped after the first 100 tickets because the loop guarded on `len(tickets) < 100` only — pages exactly 100 long were treated as final. Round-22 keys the loop on the `Link: rel=\"next\"` header so any size of final page is detected.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/freshdesk",
			TestName: "TestFreshdesk_DeltaSync_Incremental",
			Source:   "internal/connector/freshdesk/freshdesk_test.go",
		}, {
			PkgRel:   "internal/connector/freshdesk",
			TestName: "TestFreshdesk_Lifecycle",
			Source:   "internal/connector/freshdesk/freshdesk_test.go",
		}},
	},
	{
		PR:      "round-22",
		Finding: 3,
		Title:   "Qdrant client validates configuration before reaching the wire",
		Symptom: "Round-21 returned a bare `qdrant: status=503` on misconfigured callers and the alert manager could not tell `not ready yet` from `bad config`. Round-22 surfaces a typed validation error before the network call so the connector health page distinguishes the two failure modes.",
		Tests: []TestRef{{
			PkgRel:   "internal/storage",
			TestName: "TestQdrant_NewQdrantClient_Validation",
			Source:   "internal/storage/qdrant_test.go",
		}},
	},
	{
		PR:      "round-22",
		Finding: 4,
		Title:   "ConnectorHealthHandler aggregates per-connector-type health from the source repo",
		Symptom: "Round-21 added `GET /v1/admin/connectors/health` but the handler returned an empty list because it walked `sources` rather than the active-source aggregate. Round-22 aggregates by connector type and excludes paused/removed sources from active counts.",
		Tests: []TestRef{{
			PkgRel:   "internal/admin",
			TestName: "TestConnectorHealthHandler_AggregatesByConnectorType",
			Source:   "internal/admin/connector_health_handler_test.go",
		}, {
			PkgRel:   "internal/admin",
			TestName: "TestConnectorHealthHandler_ExcludesPausedRemovedFromActiveStats",
			Source:   "internal/admin/connector_health_handler_test.go",
		}},
	},
	{
		PR:      "round-23",
		Finding: 5,
		Title:   "Intercom DeltaSync branches on namespace (conversations vs articles)",
		Symptom: "Round-20 hard-coded `/conversations` even when the caller asked for `articles`; Round-23 routes by namespace ID so both surfaces can be ingested side-by-side.",
		Tests: []TestRef{{
			PkgRel:   "internal/connector/intercom",
			TestName: "TestIntercom_DeltaSync_Incremental",
			Source:   "internal/connector/intercom/intercom_test.go",
		}, {
			PkgRel:   "internal/connector/intercom",
			TestName: "TestIntercom_Lifecycle",
			Source:   "internal/connector/intercom/intercom_test.go",
		}},
	},
	{
		PR:      "round-23",
		Finding: 6,
		Title:   "Paused sources are filtered out of the worker fan-out",
		Symptom: "Round-22 added the `paused` source state but the worker still picked them up because the fan-out query did not filter on the lifecycle column. Round-23 adds the filter and three regression tests that assert paused sources never reach the dispatch channel.",
		Tests: []TestRef{{
			PkgRel:   "internal/pipeline",
			TestName: "TestCoordinator_PausedSourceFilter_SkipsFetch",
			Source:   "internal/pipeline/paused_source_filter_test.go",
		}, {
			PkgRel:   "internal/pipeline",
			TestName: "TestCoordinator_PausedSourceFilter_OnlyPausedSourceIsSkipped",
			Source:   "internal/pipeline/paused_source_filter_test.go",
		}, {
			PkgRel:   "internal/pipeline",
			TestName: "TestCoordinator_PausedSourceFilter_RecordsSyncStart",
			Source:   "internal/pipeline/paused_source_filter_test.go",
		}},
	},
}
