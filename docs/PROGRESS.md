# hunting-fishball — Progress

This document tracks the *actual* state of the platform. The shape mirrors
[`PHASES.md`](PHASES.md). Anything not marked here is `⏳ planned`.

| Status legend |  |
|---------------|--|
| ✅ shipped | Landed in `main` and exercised in production |
| 🟡 partial | Some criteria met; gaps listed inline |
| ⏳ planned | Not yet started |

> **Reminder.** This is a greenfield repository. Until interfaces and
> services exist, every box is unchecked.

---

## Phase 0 — Connector contract, registry, audit primitives

**Status.** 🟡 partial | ~100% (every exit criterion met; phase marker stays `partial` for cross-phase invariants until Phase 4/5/6 client work lands)

- [x] `SourceConnector` interface defined in the platform backend
- [x] Optional `DeltaSyncer` / `WebhookReceiver` / `Provisioner` interfaces
- [x] Process-global connector registry (`Register*` / `Get*`)
- [x] AES-GCM credential encryption reused from the platform backend
- [x] Audit-log Postgres table + Kafka outbox
- [x] Audit-log API surfaced to the admin portal

## Phase 1 — Single-source MVP end-to-end

**Status.** 🟡 partial | ~100% (every exit criterion met and exercised by the docker-compose e2e smoke + benchmark P95 budget)

- [x] Google Drive connector implements `SourceConnector` with delta tokens
- [x] Slack connector implements `SourceConnector` with the Events API
- [x] Go context engine consumes from Kafka
- [x] Stage 1 (Fetch) Go worker
- [x] Stage 2 (Parse) gRPC integration with Python Docling service
- [x] Stage 3 (Embed) gRPC integration with Python embedding service
- [x] Stage 4 (Storage) Go worker — Qdrant + Postgres
- [x] `POST /v1/retrieve` returns top-k matches from Qdrant
- [x] CI end-to-end smoke test (docker-compose storage plane)

## Phase 2 — B2B Admin Source Management

**Status.** 🟡 partial | ~100% (every exit criterion met; forget worker and per-tenant routing covered by e2e + isolation smoke)

- [x] Admin portal flows for connect / pause / re-scope / remove
      (`internal/admin/source_handler.go`)
- [x] Per-tenant Kafka topic routing keyed on `tenant_id || source_id`
      (`internal/pipeline/producer.go`,
      `internal/pipeline/consumer.go::ParsePartitionKey`)
- [x] Org-wide sync pipeline runs through the Go context engine
      (`internal/pipeline/backfill.go` distinguishes `backfill`
      vs `steady` via `IngestEvent.SyncMode`)
- [x] Per-source quota + rate-limit enforcement at the platform
      backend (`internal/admin/ratelimit.go`, Redis token bucket)
- [x] Sync health (last-success, lag, error counts) surfaced in
      admin UI (`internal/admin/health.go`,
      `migrations/003_source_health.sql`)
- [x] Forget-on-removal worker — fenced lease prevents re-add races
      (`internal/admin/forget_worker.go`)
- [x] Connector lifecycle events emitted to audit log
      (`internal/audit/model.go::Action*`)

## Phase 3 — Retrieval fan-out

**Status.** 🟡 partial | ~100% (every exit criterion met; P95 budget enforced by `tests/benchmark/p95_e2e_test.go`)

- [x] Qdrant Go client integrated, per-tenant collections
- [x] BM25 search integrated via `bleve` (pure-Go fallback for
      `tantivy-go`, per-tenant index directories)
- [x] FalkorDB Go client integrated, per-tenant graphs
- [x] Mem0 gRPC contract + Go client (wired through retrieval handler)
- [x] Retrieval API parallel fan-out via `errgroup` with per-backend
      deadlines and `policy.degraded` signalling
- [x] Reciprocal-rank fusion merger
- [x] Lightweight Go-side reranker (BM25 blend + freshness boost) +
      `Reranker` interface for a future cross-encoder
- [x] Redis semantic cache with per-tenant key prefix and explicit
      `Invalidate` on Stage 4 writes
- [x] Retrieval P95 < 500 ms on the sample corpus —
      in-process P95 ~178 µs measured in `tests/benchmark/`;
      end-to-end budget enforced by
      `tests/benchmark/p95_e2e_test.go::TestE2E_RetrieveP95`
      (200-query sample, fails the test if P95 > 500 ms or
      round-trip P95 > 1 s, runs under `make bench-e2e`).
      Retrieval optimisations: Qdrant connection pre-warm
      (`internal/storage/qdrant.go::Warmup`), FalkorDB keep-alive
      ticker (`internal/storage/falkordb.go::KeepAlive`), and a
      pipelined `SemanticCache.Set` continue to keep the long-pole
      tail bounded. Per-backend timing is exposed on
      `RetrieveResponse.Timings` (vector_ms / bm25_ms / graph_ms /
      memory_ms / merge_ms / rerank_ms) so operators can identify
      the long pole without reaching for traces.

## Phase 4 — Policy framework + simulator + privacy strip

**Status.** 🟡 partial | ~98% — the sole remaining checkbox is client-side rendering, tracked in external B2C / desktop repositories (`uneycom/b2c-kchat-portal`, `uneycom/skytrack-*`).

- [x] Tenant- and channel-scoped privacy mode
      (`internal/policy/privacy_mode.go`,
      `internal/policy/EffectiveMode` returns the stricter of the two)
- [x] Allow / deny lists (source / namespace / path glob)
      (`internal/policy/acl.go`,
      `internal/retrieval/policy_snapshot.go::applyPolicySnapshot`)
- [x] Recipient policy (channel / skill)
      (`internal/policy/recipient.go`, gated in
      `internal/retrieval/handler.go` after merge + rerank)
- [x] What-if simulator (`internal/policy/simulator.go`,
      copy-on-write `PolicySnapshot.Clone()` so the live cache is
      never aliased)
- [x] Data-flow diff in simulator (`internal/policy/simulator_diff.go`,
      per-privacy-tier counts + percentage delta)
- [x] Conflict detection in simulator
      (`internal/policy/simulator_conflict.go`, three categories:
      `privacy_mode_override`, `acl_overlap`,
      `recipient_contradiction`; deterministic ordering for
      reproducible admin-portal renders)
- [x] Drafts isolated; explicit promotion audited
      (`internal/policy/draft.go`, `migrations/005_policy_drafts.sql`,
      `internal/policy/promotion.go` with `policy.promoted` /
      `policy.rejected` audit events; `internal/policy/live_store.go`
      writes the live tables transactionally; the
      `AuditWriter.CreateInTx` port pulls the audit row inside the
      same `*gorm.DB` transaction so a `LiveStore` failure rolls the
      audit log back with the rest)
- [x] `privacy_label` returned on every retrieval row
- [x] Privacy strip enrichment in retrieval response
      (`internal/retrieval/privacy_strip.go`, every `RetrieveHit`
      carries `mode` / `processed_where` / `model_tier` /
      `data_sources` / `policy_applied`)
- [x] Admin HTTP surface for drafts + simulator
      (`internal/admin/simulator_handler.go` mounts
      `POST/GET /v1/admin/policy/drafts`,
      `POST /v1/admin/policy/drafts/:id/promote|reject`,
      `POST /v1/admin/policy/simulate`,
      `POST /v1/admin/policy/simulate/diff`,
      `POST /v1/admin/policy/conflicts`)
- [x] Simulator wired to live state in `cmd/api`:
      `policy.NewLiveResolverGORM` reads the live policy tables
      defined in `migrations/004_policy.sql`, and the simulator's
      `Retriever` delegates to
      `retrieval.Handler.RetrieveWithSnapshot` so what-if /
      data-flow diff calls run the full fan-out + merge + rerank
      pipeline against the draft snapshot.
- [ ] Privacy strip rendered in admin portal, desktop, and mobile UIs
      (server-side enrichment shipped; client-side rendering tracked
      separately under Phase 6)

## Phase 5 — On-device knowledge core integration

**Status.** 🟡 partial | ~85% — all server-side work complete through Round 14 (shard manifest API, generation worker, delta sync, coverage endpoint, forgetting orchestrator, model catalog, eviction policy). On-device implementations are tracked in `kennguy3n/knowledge`.

- [x] UniFFI XCFramework for iOS — server-side contract
      (`docs/contracts/uniffi-ios.md`,
      `internal/shard/contract.go::ShardClientContract`); the
      `kennguy3n/knowledge` Rust crate must implement the
      mirrored interface and ship the XCFramework.
- [x] UniFFI AAR for Android — server-side contract
      (`docs/contracts/uniffi-android.md`); shares
      `ShardClientContract` with iOS.
- [x] N-API binding for desktop — server-side contract
      (`docs/contracts/napi-desktop.md`); shares
      `ShardClientContract` with iOS / Android.
- [x] On-device retrieval shard sync — server-side
      (`internal/shard/handler.go` mounts
      `GET /v1/shards/:tenant_id` and
      `GET /v1/shards/:tenant_id/delta?since=<v>`;
      `internal/shard/repository.go` is the GORM-backed metadata
      store; `migrations/006_shards.sql` defines `shards`;
      `GET /v1/shards/:tenant_id/coverage` exposes shard /
      corpus chunk counts so clients can run the local-first
      decision tree per `docs/contracts/local-first-retrieval.md`)
- [x] Shard generation worker — policy-aware
      (`internal/shard/generator.go` calls `PolicyResolver.Resolve`
      to gate eligible chunks; wired into `cmd/ingest/main.go` as
      an optional post-Stage-4 hook for `shard.requested`)
- [x] Shard delta sync protocol — version-keyed add / remove
      (`internal/shard/delta.go`)
- [x] Local-first retrieval contract —
      `docs/contracts/local-first-retrieval.md` documents the
      decision tree; `GET /v1/shards/:tenant_id/coverage` and
      the `prefer_local` hint in `RetrieveResponse` are the
      two server endpoints clients consume. On-device
      enforcement still lives in `kennguy3n/knowledge`.
- [x] Bonsai-1.7B integration contract —
      `docs/contracts/bonsai-integration.md` +
      `internal/models/` ship the model catalog (3 Bonsai-1.7B
      builds at q4_0 / q8_0 / fp16 with per-tier eviction
      config). `GET /v1/models/catalog` is the client-facing
      endpoint. Actual `llama.cpp` integration runs in
      `kennguy3n/knowledge` and `kennguy3n/llama.cpp`.
- [x] Cryptographic forgetting on the on-device tier — server-side
      (`internal/shard/forget.go` orchestrates pending_deletion →
      drain → drop Qdrant / FalkorDB / Tantivy / Redis →
      destroy DEKs → mark deleted; `cmd/api/main.go` mounts
      `DELETE /v1/tenants/:tenant_id/keys`)

## Phase 6 — B2C client surfaces

**Status.** 🟡 partial | ~75% — server contracts and endpoints are complete (`internal/b2c/`, device-first hint, privacy-strip enrichment, `/v1/sync/schedule`). Client UI development is tracked in B2C repos (`uneycom/b2c-kchat-portal`, `uneycom/skytrack-*`).

- [x] iOS / Android / desktop B2C apps consume the same retrieval API —
      server-side SDK contract (`docs/contracts/b2c-retrieval-sdk.md`)
      backed by `internal/b2c/handler.go` mounting
      `GET /v1/health` and `GET /v1/capabilities`. The capabilities
      endpoint reports enabled backends, supported privacy modes,
      and the `device_first` / `local_shard_sync` feature flags so
      a B2C UI built against an older server can downgrade
      gracefully.
- [x] On-device first by default — `internal/policy/device_first.go`
      (`Decide`) and `RetrieveResponse.prefer_local` /
      `local_shard_version` / `prefer_local_reason` echo the hint
      to clients. Wired through `cmd/api/main.go` via
      `shard.VersionLookup` against the shard repository so the
      hint reflects the freshest shard version.
- [x] Privacy strip render contract for B2C clients —
      `docs/contracts/privacy-strip-render.md` documents the JSON
      shape and per-platform render guidance. Server-side
      enrichment shipped in Phase 4.
- [x] Background sync per platform native scheduler —
      `docs/contracts/background-sync.md` documents iOS
      `BGAppRefreshTask`, Android `WorkManager`, and Electron
      `powerMonitor` integration; `GET /v1/sync/schedule`
      (`internal/b2c/handler.go`) is the server-side scheduler
      hint.

## Phase 7 — Catalog expansion

**Status.** ✅ shipped | ~100% — Round 20/21 expands the catalog from 42 → **50** production connectors (Zendesk, ServiceNow, Freshdesk, Airtable, Trello, Intercom, Webex, Bitbucket on top of the Round 18 set). Per-connector runbooks under `docs/runbooks/`, end-to-end smoke green per connector (`tests/e2e/connector_smoke_test.go`, `make test-connector-smoke`), capability matrix below.

- [x] ≥ 12 production connectors at GA — Phase 1 (Google Drive,
      Slack, KChat) + Phase 7 (SharePoint, OneDrive, Dropbox,
      Box, Notion, Confluence, Jira, GitHub, GitLab, Microsoft
      Teams) + Round-15 Phase-2+ adds (S3, Linear, Asana, Discord,
      Salesforce, HubSpot, Google Shared Drives) + Round-16
      Phase-2+ adds (Mattermost, ClickUp, Monday.com, Pipedrive,
      Okta, Gmail, RSS/Atom, Confluence Server/DC) +
      Round-17 Phase-2+ adds (Microsoft Entra ID, Google
      Workspace Directory, Microsoft 365 Outlook, Workday,
      BambooHR, Personio, Sitemap, Coda) + Round-18 Phase-2+
      adds (SharePoint on-prem, Azure Blob, Google Cloud Storage,
      Egnyte, BookStack, signed-upload portal) + Round-20/21
      Phase-2+ adds (Zendesk, ServiceNow, Freshdesk, Airtable,
      Trello, Intercom, Webex, Bitbucket) = **50**
- [x] Per-connector runbooks (`docs/runbooks/` —
      one Markdown file per connector covering credential rotation,
      quota / rate-limit incidents, outage detection / recovery, and
      common error codes)
- [x] Per-connector capability matrix in this doc (see below)
- [x] End-to-end smoke test green per connector
      (`tests/e2e/connector_smoke_test.go`, build tag `e2e`,
      runs Validate → Connect → ListNamespaces → ListDocuments →
      FetchDocument plus DeltaSync / HandleWebhook where supported;
      `make test-connector-smoke`)

## Phase 8 — Cross-platform optimization

**Status.** ✅ shipped | ~100% — all 12 exit criteria items shipped (Go context-engine tuning, Python ML scaling, cross-platform on-device benchmarks + eviction contract). Round 14 hardens the production surface on top of this phase rather than reopening it.

Go context engine tuning:

- [x] Goroutine pool sizing per stage — `pipeline.StageConfig` adds
      `FetchWorkers` / `ParseWorkers` / `EmbedWorkers` /
      `StoreWorkers` to `CoordinatorConfig`; the coordinator
      replaces the original "1 goroutine per stage" topology with
      bounded worker pools that close downstream channels via a
      `sync.WaitGroup` once every stage worker has exited
      (`internal/pipeline/coordinator.go`)
- [x] Kafka consumer rebalancing tuned — `pipeline.ConsumerTuning`
      exposes `SessionTimeout`, `MaxPollInterval`, and
      `RebalanceStrategy` (sticky / range / roundrobin); defaults
      stay sticky to preserve per-source ordering across
      rebalances (`internal/pipeline/consumer.go::SaramaConfigWith`)
- [x] Connection pooling for Qdrant / FalkorDB / Tantivy / Postgres
      — Qdrant uses a sized `http.Transport` with
      `MaxIdleConnsPerHost` tunable via
      `CONTEXT_ENGINE_QDRANT_POOL_SIZE`; the FalkorDB-shared Redis
      pool is sized via `CONTEXT_ENGINE_REDIS_POOL_SIZE`; the
      Postgres pool sets `SetMaxOpenConns` /
      `SetMaxIdleConns` / `SetConnMaxLifetime` from
      `CONTEXT_ENGINE_PG_MAX_OPEN` and
      `CONTEXT_ENGINE_PG_MAX_IDLE`
- [x] HPA on Kafka lag (ingest) and QPS (api) —
      `deploy/hpa-api.yaml` scales `context-engine-api` on CPU
      and `context_engine_api_requests_per_second`;
      `deploy/hpa-ingest.yaml` scales `context-engine-ingest` on
      CPU and `context_engine_kafka_consumer_lag`. Metrics are
      exposed at `/metrics` from
      `internal/observability/metrics.go` (registered via
      `prometheus.NewRegistry`), the API binary recording per-
      request count + duration via
      `observability.PrometheusMiddleware`, the ingest binary
      reporting Kafka lag from `internal/pipeline/consumer.go`
      after each commit. Per-stage pipeline duration is recorded
      via `observability.ObserveStageDuration` from
      `internal/pipeline/coordinator.go::runWithRetry`. Per-
      backend retrieval duration is recorded via
      `observability.ObserveBackendDuration` in
      `internal/retrieval/handler.go::fanOut`. Liveness
      (`/healthz`) and readiness (`/readyz`) probes ship on the
      same listener (`cmd/api/readyz.go`,
      `cmd/ingest/health.go`).
- [x] OpenTelemetry trace sampling tuned for cost / tail-latency
      tradeoff — `internal/observability/tracing.go` centralises
      tracer + attribute keys; spans emitted around the four
      pipeline stages and the four retrieval backends
      (vector / bm25 / graph / memory) with hit-count + latency_ms
      attributes; `RetrieveResponse.TraceID` echoes the trace_id
      to the client per the API contract

Python ML microservice scaling:

- [x] HPA on Docling worker (CPU + queue depth) —
      `deploy/hpa-docling.yaml` scales the docling deployment on
      CPU, memory, and the `docling_parse_queue_depth` Prometheus
      gauge exported from `services/docling/docling_server.py`
      (the gauge increments on every gRPC call via the shared
      `services/_metrics.py::ServiceMetrics.time` context manager).
      Sidecar `/metrics` HTTP listener defaults to port 9090
      (`METRICS_PORT` env var override).
- [x] HPA on embedding worker (CPU + queue depth) —
      `deploy/hpa-embedding.yaml` scales the embedding deployment
      on CPU, memory, and `embedding_queue_depth` exported from
      `services/embedding/embedding_server.py` via the same shared
      `services/_metrics.py` collectors.
- [x] Mem0 partitioning by tenant prefix —
      `services/memory/memory_server.py::tenant_prefix` resolves a
      tenant_id to a configurable Mem0 partition prefix (template
      defaults to `"{tenant_id}"`, override via
      `MEM0_TENANT_PREFIX_TEMPLATE`). Every Mem0 `add` /
      `search` keys on `<prefix>:<user_id>` and metadata records
      both `tenant_id` and `tenant_prefix`; `search` also drops
      stray rows whose metadata `tenant_id` mismatches as a
      defence-in-depth guard. Cross-tenant isolation verified by
      `services/memory/test_partitioning.py`. The Go memory client
      (`cmd/api/main.go::memoryAdapter`) passes `tenant_id` on
      every `SearchMemory` call (verified by
      `cmd/api/memory_adapter_test.go`).
- [x] gRPC connection pooling + per-target deadlines on the Go side
      — `internal/grpcpool/` provides a round-robin pool with
      configurable `Deadline`, a `Threshold`-based circuit
      breaker (closed → open → half-open → closed) and
      `OpenFor` recovery window
- [x] Capacity test (N docs / min) without back-pressure to
      connectors — `tests/capacity/capacity_test.go` submits
      configurable docs/minute through the coordinator with fake
      stages and asserts every submit completes within the
      submit deadline (no producer back-pressure); `make
      capacity-test` runs it; `CAPACITY_DOCS_PER_MIN` /
      `CAPACITY_DURATION` tune the load shape

Cross-platform on-device:

- [x] Bonsai-1.7B benchmarks across ≥ 3 tiers per platform —
      benchmark contract published in
      `tests/benchmark/bonsai_contract_test.go::BonsaiContract`
      defining min tokens/sec, max first-token latency, and max
      memory per tier. Actual on-device measurements run in
      `kennguy3n/knowledge` + `kennguy3n/llama.cpp` against this
      contract.
- [x] On-device retrieval shard eviction tuned per tier —
      `internal/shard/eviction.go` ships `EvictionPolicy` /
      `ShouldEvict` with `unknown_tier` / `shard_too_large` /
      `memory_pressure` decision labels and a thermal-throttle
      multiplier. Defaults shipped via
      `DefaultEvictionPolicies()` (Low: 64 MB / 256 MB free,
      Mid: 256 MB / 512 MB free, High: 1024 MB / 1024 MB free,
      0.5x multiplier on thermal). Surfaced to clients in the
      `eviction_config` field of `GET /v1/models/catalog`.

---

## Context Engine migration tasks

These are the discrete tasks that move the context engine from
"Python pipeline + Python retrieval" to "Go orchestrator + Python ML
microservices behind gRPC". They cross-cut Phases 1–3 and 8.

### Proto / contract definitions

- [x] Define gRPC proto files for the document parsing service (Docling)
- [x] Define gRPC proto files for the embedding computation service
- [x] Define gRPC proto files for the memory service (Mem0)

### Go context engine — pipeline

- [x] Implement Go Kafka consumer (replacing Python `FastKafkaConsumer`)
- [x] Implement Go pipeline coordinator with goroutine-based workers
      (replacing Python's `multiprocessing` `ProcessCoordinator`)
- [x] Implement Go Stage 1 (Fetch) worker — HTTP / S3 + retry / dedupe
- [x] Implement Go Stage 4 (Storage) worker — Qdrant + PostgreSQL writes
      (FalkorDB lands in Phase 3)

### Go context engine — retrieval API

- [x] Implement Go retrieval API with Gin
- [x] Implement Go vector search client (Qdrant)
- [x] Implement Go BM25 search (pure-Go `bleve` fallback for
      `tantivy-go`)
- [x] Implement Go graph traversal client (FalkorDB)
- [x] Implement Go semantic cache (Redis)
- [x] Implement Go result merger and reranker

### Python ML microservices

- [x] Build Python Docling gRPC microservice (thin wrapper)
- [x] Build Python embedding gRPC microservice (thin wrapper)
- [x] Build Python Mem0 gRPC microservice (thin wrapper)

### Validation

- [x] Write integration tests for Go ↔ Python gRPC communication
- [x] Benchmark Go context engine vs Python baseline
      (throughput, P50 / P95 / P99 latency, RSS per worker)
- [x] Document cutover plan and rollback procedure

---

## Connector capability matrix

The catalog expands per [`PHASES.md`](PHASES.md) Phase 7. Until Phase 1
ships, the matrix is empty. Each row records:

- **Identity** — pull users / groups
- **Documents** — pull document content / metadata
- **Webhooks** — receive change events without polling
- **Delta** — supports delta tokens / change cursors
- **Provisioning** — push grants / changes back to the source

| Connector | Identity | Documents | Webhooks | Delta | Provisioning | Status |
|---|---|---|---|---|---|---|
| Google Drive | ❌ | ✅ | ❌ (poll) | ✅ (`changes.list` cursor) | ❌ | 🟡 Phase 1 |
| Slack        | ✅ (workspace users) | ✅ (channels + threads) | ✅ (Events API) | ✅ (`oldest`/`latest` cursor) | ❌ | 🟡 Phase 1 |
| SharePoint Online | ❌ | ✅ (drive items) | ❌ | ✅ (Graph delta token) | ❌ | 🟡 Phase 7 |
| OneDrive     | ❌ | ✅ (drive items) | ❌ | ✅ (Graph delta token) | ❌ | 🟡 Phase 7 |
| Dropbox      | ❌ | ✅ (file entries) | ❌ | ✅ (`list_folder/continue` cursor) | ❌ | 🟡 Phase 7 |
| Box          | ❌ | ✅ (file entries) | ❌ | ✅ (events stream `next_stream_position`) | ❌ | 🟡 Phase 7 |
| Notion       | ❌ | ✅ (pages) | ❌ | ✅ (`last_edited_time` filter) | ❌ | 🟡 Phase 7 |
| Confluence Cloud | ❌ | ✅ (pages) | ❌ | ✅ (CQL `lastModified`) | ❌ | 🟡 Phase 7 |
| Jira Cloud   | ❌ | ✅ (issues) | ✅ (Jira webhooks) | ✅ (JQL `updated`) | ❌ | 🟡 Phase 7 |
| GitHub       | ❌ | ✅ (issues / PRs) | ✅ (GitHub webhooks) | ✅ (`since` filter) | ❌ | 🟡 Phase 7 |
| GitLab       | ❌ | ✅ (issues) | ✅ (GitLab webhooks) | ✅ (`updated_after`) | ❌ | 🟡 Phase 7 |
| Microsoft Teams | ❌ | ✅ (channel messages) | ✅ (Graph change notifications) | ✅ (`messages/delta`) | ❌ | 🟡 Phase 7 |
| KChat        | ✅ (workspace users via `/users.me`) | ✅ (channels + messages) | ✅ (KChat Events API with HMAC verification) | ✅ (`/channels.changes` cursor) | ❌ | ✅ Round-15 (Phase 1) |
| S3-compatible | ❌ | ✅ (objects under bucket / prefix) | ❌ | ✅ (`start-after` key / last-modified filter) | ❌ | ✅ Round-15 (Phase 2+) |
| Linear       | ✅ (`viewer` GraphQL) | ✅ (issues by team) | ✅ (Linear webhooks with HMAC) | ✅ (`updatedAt >` filter) | ❌ | ✅ Round-15 (Phase 2+) |
| Asana        | ✅ (`/users/me`) | ✅ (tasks by project) | ❌ (poll) | ✅ (`modified_since`) | ❌ | ✅ Round-15 (Phase 2+) |
| Discord      | ✅ (`/users/@me`) | ✅ (channel messages) | ❌ (Gateway out of scope) | ✅ (`after` snowflake cursor) | ❌ | ✅ Round-15 (Phase 2+) |
| Salesforce   | ❌ | ✅ (SObject records via SOQL) | ❌ (Platform Events planned) | ✅ (`SystemModstamp` filter) | ❌ | ✅ Round-15 (Phase 2+) |
| HubSpot      | ❌ | ✅ (CRM objects: contacts, companies, deals, tickets) | ❌ (planned) | ✅ (`hs_lastmodifieddate` search) | ❌ | ✅ Round-15 (Phase 2+) |
| Google Shared Drives | ❌ | ✅ (files in shared drives only) | ❌ | ✅ (`changes.list` cursor with `supportsAllDrives`) | ❌ | ✅ Round-15 (Phase 2+) |
| Mattermost   | ✅ (`/api/v4/users/me`) | ✅ (channel posts) | ❌ (planned) | ✅ (`since=<ms-epoch>`) | ❌ | ✅ Round-16 (Phase 2+) |
| ClickUp      | ✅ (`/api/v2/user`) | ✅ (tasks by list/folder) | ❌ (planned) | ✅ (`date_updated_gt`) | ❌ | ✅ Round-16 (Phase 2+) |
| Monday.com   | ✅ (`me { id }`) | ✅ (items by board) | ❌ (planned) | ✅ (`__last_updated__` column) | ❌ | ✅ Round-16 (Phase 2+) |
| Pipedrive    | ✅ (`/users/me`) | ✅ (deals / persons / activities) | ❌ (planned) | ✅ (`/recents?since_timestamp`) | ❌ | ✅ Round-16 (Phase 2+) |
| Okta         | ✅ (`/api/v1/users/me`) | ✅ (users + groups) | ❌ (Event Hooks planned) | ✅ (`filter=lastUpdated gt`) | ❌ | ✅ Round-16 (Phase 2+, Identity) |
| Gmail        | ❌ | ✅ (messages) | ❌ (Pub/Sub planned) | ✅ (`history.list` historyId) | ❌ | ✅ Round-16 (Phase 2+, Email) |
| RSS / Atom   | ❌ | ✅ (entries) | ❌ (poll-only) | ✅ (max `<pubDate>` / `<updated>`) | ❌ | ✅ Round-16 (Phase 2+, Generic) |
| Confluence Server / DC | ✅ (`/rest/api/user/current`) | ✅ (pages by space) | ❌ (planned) | ✅ (CQL `lastModified`) | ❌ | ✅ Round-16 (Phase 2+, Knowledge) |
| Microsoft Entra ID | ✅ (users + groups via Graph) | ✅ (users + groups) | ❌ (Change Notifications planned) | ✅ (Graph `$deltatoken` on `/users/delta` + `/groups/delta`) | ✅ (`accountEnabled=false`, `@removed` → ChangeDeleted) | ✅ Round-17 (Phase 2+, Identity) |
| Google Workspace Directory | ✅ (users + groups) | ✅ (users + groups) | ❌ (Reports API webhooks planned) | ✅ (`updatedMin=<RFC3339>` filter) | ✅ (`suspended=true` → ChangeDeleted) | ✅ Round-17 (Phase 2+, Identity) |
| Microsoft 365 Outlook | ❌ | ✅ (messages by folder) | ❌ (Graph Change Notifications planned) | ✅ (Graph `@odata.deltaLink` on `/messages/delta`) | ❌ | ✅ Round-17 (Phase 2+, Email) |
| Workday | ❌ | ✅ (workers) | ❌ (no public webhook) | ✅ (`Updated_From=<RFC3339>` filter) | ✅ (`active=false`, `terminationDate` → ChangeDeleted) | ✅ Round-17 (Phase 2+, HR) |
| BambooHR | ❌ | ✅ (employee directory) | ❌ | ✅ (`/v1/employees/changed?since=<ISO8601>`) | ✅ (`action="Deleted"` → ChangeDeleted) | ✅ Round-17 (Phase 2+, HR) |
| Personio | ❌ | ✅ (employees) | ❌ | ✅ (`updated_from=<RFC3339>`) | ✅ (`status="inactive"` → ChangeDeleted) | ✅ Round-17 (Phase 2+, HR) |
| Sitemap crawl | ❌ | ✅ (URLs in `<urlset>` / `<sitemapindex>`) | ❌ | ✅ (`<lastmod>` timestamp comparison) | ❌ | ✅ Round-17 (Phase 2+, Generic) |
| Coda | ✅ (`/whoami`) | ✅ (docs + pages) | ❌ (planned) | ✅ (`sortBy=updatedAt&direction=DESC` walk) | ❌ | ✅ Round-17 (Phase 2+, Knowledge) |
| SharePoint on-prem | ✅ (`/_api/web/lists`) | ✅ (lists + items) | ❌ (Modified-cursor only) | ✅ (`Modified gt datetime'<ISO8601>'`) | ❌ | ✅ Round-18 (Phase 2+, ECM) |
| Azure Blob | ✅ (SAS) | ✅ (blobs in container) | ❌ (manifest pull) | ✅ (`x-ms-blob-last-modified` cursor) | ❌ | ✅ Round-18 (Phase 2+, Storage) |
| Google Cloud Storage | ✅ (OAuth bearer) | ✅ (objects in bucket) | ❌ | ✅ (`timeCreated`/`updated` filter) | ❌ | ✅ Round-18 (Phase 2+, Storage) |
| Egnyte | ✅ (OAuth) | ✅ (`/pubapi/v1/fs/<path>`) | ❌ | ✅ (events `/pubapi/v2/events/cursor`) | ❌ | ✅ Round-18 (Phase 2+, ECM) |
| BookStack | ✅ (Token-ID + Token-Secret) | ✅ (`/api/pages`) | ❌ | ✅ (`updated_at` filter + sort) | ❌ | ✅ Round-18 (Phase 2+, Knowledge) |
| Upload portal | ✅ (HMAC) | ➖ webhook receiver | ❌ | ➖ webhook-driven | ✅ (signed-URL upload + HMAC sig) | ✅ Round-18 (Phase 2+, Receiver) |
| Zendesk | ✅ (API token / OAuth bearer) | ✅ (tickets + Help Center articles) | ❌ (planned) | ✅ (`/api/v2/incremental/tickets.json?start_time=<unix>`) | ❌ | ✅ Round-20 (Phase 2+, Support) |
| ServiceNow | ✅ (Basic / OAuth) | ✅ (incident / change / kb_knowledge via `/api/now/table`) | ❌ | ✅ (`sysparm_query=sys_updated_on>javascript:gs.dateGenerate(...)`) | ❌ | ✅ Round-20 (Phase 2+, ITSM) |
| Freshdesk | ✅ (API key as basic-auth user) | ✅ (tickets via `/api/v2/tickets`) | ❌ | ✅ (`updated_since=<ISO8601>` + `page`) | ❌ | ✅ Round-20 (Phase 2+, Support) |
| Airtable | ✅ (PAT / OAuth bearer) | ✅ (records per `/v0/<base>/<table>`) | ❌ | ✅ (`filterByFormula=LAST_MODIFIED_TIME()>'<ISO8601>'` + `offset`) | ❌ | ✅ Round-20 (Phase 2+, DB) |
| Trello | ✅ (API key + token query) | ✅ (cards by board) | ❌ | ✅ (`/1/boards/<id>/actions?since=<ISO8601>` + `before`) | ❌ | ✅ Round-20 (Phase 2+, PM) |
| Intercom | ✅ (Bearer token) | ✅ (conversations + articles) | ❌ (planned) | ✅ (`POST /conversations/search` with `updated_at > <unix>`) | ❌ | ✅ Round-20 (Phase 2+, Support) |
| Webex | ✅ (Bearer token) | ✅ (messages by room) | ❌ (planned) | ✅ (`/v1/messages?roomId=<id>` + `before`/`max`) | ❌ | ✅ Round-20 (Phase 2+, Chat) |
| Bitbucket | ✅ (App password / OAuth) | ✅ (pullrequests + issues per `/2.0/repositories/<ws>/<repo>`) | ❌ (planned) | ✅ (`q=updated_on>"<ISO8601>"` + `pagelen` + `next`) | ❌ | ✅ Round-20 (Phase 2+, VCS) |

---

## Changelog

- 2026-05-13: **Round 20/21: Connector catalog expansion — 42 →
  50 production connectors + production hardening + CI lane
  optimisation.** Phase A Tasks 1-8 add Zendesk (Support REST
  API, `/api/v2/incremental/tickets.json?start_time=<unix>`
  incremental-export cursor + `Retry-After` 429), ServiceNow
  (Table API basic/OAuth against `{instance}.service-now.com`,
  `sysparm_query=sys_updated_on>javascript:gs.dateGenerate(...)`
  cursor with `sysparm_offset` pagination), Freshdesk (API key as
  basic-auth user with password `X`, `/api/v2/tickets` +
  `updated_since` ISO-8601 + `page`), Airtable (Bearer PAT/OAuth
  against `/v0/<baseId>/<tableIdOrName>` with
  `filterByFormula=LAST_MODIFIED_TIME()>'<ISO8601>'` and
  `offset` continuation), Trello (API key + token query,
  `/1/boards/<id>/cards` + `/1/boards/<id>/actions?since` with
  `before` cursor), Intercom (Bearer, `POST /conversations/search`
  with `updated_at > <unix>` plus `/articles` walk), Webex
  (Bearer Bot/OAuth, `/v1/messages?roomId=<id>` + `before`/`max`
  cursor pagination), and Bitbucket Cloud (App password / OAuth,
  `/2.0/repositories/<ws>/<repo>/pullrequests` + issues with
  `q=updated_on>"<ISO8601>"` and `next` link pagination). Each
  connector ships a stdlib `net/http` client with
  `http.NewRequestWithContext`, wraps HTTP 429 as
  `connector.ErrRateLimited`, references `connector.ErrInvalidConfig`
  / `connector.ErrNotSupported`, exposes `SourceConnector` +
  `DeltaSyncer`, and is httptest-backed with full
  Validate/Connect/ListNamespaces/ListDocuments/FetchDocument +
  DeltaSync bootstrap/incremental/429 test sweeps. Blank-imports
  added to `cmd/api/main.go` and `cmd/ingest/main.go`. Phase B
  Tasks 9-12 lift the catalogue gates: audit floor 41 → 49
  (`internal/connector/audit_test.go`), smoke registry pin
  42 → 50 (`tests/e2e/connector_smoke_test.go`) with new per-
  connector smoke entries, runbook floor 42 → 50 plus 8 new
  runbook markdown files under `docs/runbooks/` (zendesk,
  servicenow, freshdesk, airtable, trello, intercom, webex,
  bitbucket — each covering credential rotation, quota / rate-
  limit incidents, outage detection, and error codes), Round-20
  e2e suite (`tests/e2e/round20_test.go`, build tag `e2e`) with
  the 50-connector registry pin, two full-lifecycle scripts
  (Zendesk incremental export and Bitbucket PR query), and a
  429-propagation sweep across all 8 new connectors,
  Round-19/20 regression manifest (`tests/regression/round1920_manifest.go`
  + `_test.go`) cataloguing every Round-18/19/20 fix (registry
  expansion to 50, per-connector DeltaSync bootstrap contract,
  upload_portal per-connection webhook fix, azure_blob HMAC
  fix, source auto-pauser retry fix, DLQ analytics UTF-8 fix,
  semantic cache singleflight fix, cross-encoder tail demotion)
  plus a meta-test asserting every `TestRef` resolves on disk,
  and an integration contract test expansion
  (`tests/integration/connector_contract_test.go`) with compile-
  time `SourceConnector` + `DeltaSyncer` assertions for all 8
  new structs plus `TestConnectorContract_Round20_DeltaSyncerEmptyCursor`
  table-driven coverage for Zendesk incremental-export,
  ServiceNow `sys_updated_on`, and Bitbucket `q=` query bootstrap
  surfaces. Phase C Tasks 13-22 ship the production-hardening
  layer: stale-connector cleanup sweep
  (`internal/connector/bootstrap_audit_test.go`) verifying every
  connector's `DeltaSync` honours the empty-cursor → "now"
  bootstrap contract without backfilling, migration 043
  (`migrations/043_connector_sync_cursors.sql` + rollback)
  carving a dedicated `connector_sync_cursors` table out of the
  source config blob, the unified connector health dashboard
  endpoint `GET /v1/admin/connectors/health`
  (`internal/admin/connector_health_handler.go`) aggregating
  per-connector-type healthy/degraded/failing/paused counts plus
  avg lag and error rate, the connector config schema validator
  (`internal/connector/schema_validator.go` exposing
  `CredentialSchemaProvider` + `ValidateCredentialsErr` and the
  JSON-Schema subset of object/required/properties/type/enum/
  additionalProperties wired into `POST /v1/admin/sources/preview`
  before `Validate()` is called), pipeline graceful backpressure
  on auto-paused sources
  (`internal/pipeline/coordinator.go` adds the
  `PausedSourceFilter` interface so Stage 1 drains in-flight
  events cleanly without retrying against the paused upstream),
  the retrieval cache tag completeness audit
  (`internal/retrieval/cache_invalidation_test.go` extended to
  recognise both `Invalidate(chunkIDs)` and the Round-19
  `InvalidateBySources(sourceIDs)` tag-based surface, with a new
  structural test guaranteeing both methods remain on
  `SemanticCache`), the OpenAPI completeness pass adding the new
  health endpoint to `docs/openapi.yaml` and
  `docs/openapi_test.go`'s requiredPaths, the dead-code +
  `go vet` + `golangci-lint` pass (0 issues), the go.mod
  dependency audit (`go mod tidy`, `go mod verify`, `govulncheck`
  green), and Docker-compose healthcheck tuning (explicit
  `kafka-broker-api-versions.sh` healthcheck on Kafka and a TCP
  probe on Qdrant, plus a `Wait for stack to settle` loop that
  also waits on Redis and Kafka). Phase D Tasks 23-25 land the
  CI optimisation: a `detect-changes` job (`dorny/paths-filter@v3`)
  that gates `fast-connector-unit`, `fast-connector-integration`,
  and `fast-regression` behind their actual paths on PRs (with
  push/schedule/dispatch forcing them to run), the
  `fast-required` aggregator updated to treat `skipped` as
  `success` so path-filtered lanes do not block doc-only PRs,
  and a unified `${{ runner.os }}-go-build-${{ hashFiles(...) }}`
  cache key prefix across every fast-lane Go job so any job can
  prime the build cache for the others (job-specific suffixes
  retained as `restore-keys` fallback). Phase E Tasks 26-30
  refreshes PROGRESS.md / PHASES.md / ARCHITECTURE.md / README.md
  to reflect the post-Round-20/21 state.

- 2026-05-12: **Round 18/19: Connector catalog expansion — 36 →
  42 production connectors + production hardening.** Tasks 1-8
  add SharePoint Server / on-prem (NTLM / app-password against
  `/_api/web/lists`, `Modified gt datetime'<ISO8601>'` cursor),
  Azure Blob (SAS-token signed REST, blob lifecycle), Google
  Cloud Storage (OAuth bearer, JSON API `o` walk +
  `timeCreated`/`updated` filter), Egnyte (OAuth, `/pubapi/v1/fs`
  + events cursor delta), BookStack (Token-ID + Token-Secret
  header, `/api/pages` with `updated_at` sort), and the signed-
  upload portal (HMAC-verified multipart receiver implementing
  `WebhookReceiver`). Tasks 9-14 add the gRPC cross-encoder
  reranker (`proto/reranker/v1/reranker.proto` + Python sidecar
  stub in `services/reranker/`), the `QueryClassifier` for
  retrieval query routing, embedding-model versioning with
  migration `041_chunk_embedding_version.sql`, DLQ analytics
  aggregation at `GET /v1/admin/dlq/analytics`, the tenant
  onboarding wizard at `POST /v1/admin/tenants/:tenant_id/
  onboarding`, and per-chunk scoring breakdown in
  `internal/retrieval/explain.go`. Tasks 15-18 add
  `tests/e2e/round18_test.go`, `tests/regression/
  round1718_manifest*.go`, contract test expansion for the new
  connectors, and a security test suite covering credential
  redaction, cross-tenant isolation, RBAC coverage, and HMAC
  signature verification. Tasks 19-20 ship the
  `fast-govulncheck` + `fast-openapi` CI fast-lane jobs (with
  `make vulncheck`). Round 19 layers Tasks 21-30 on top —
  per-source semantic-cache invalidation
  (`SemanticCache.InvalidateBySources`) and cache-aside
  background refresh (`GetOrRefresh`), `DocumentContentType` +
  migration `042_document_content_type.sql` (multi-modal prep),
  `internal/retrieval/chunk_merger.go` (post-rerank adjacent-
  short-chunk merging gated behind
  `CONTEXT_ENGINE_CHUNK_MERGE_ENABLED`),
  `internal/admin/source_auto_pause.go` (sliding-window error-
  rate detector emitting `source.auto_paused` + Prometheus
  `SourceAutopaused` alert), bulk `reindex` + `rotate-
  credentials` actions on `POST /v1/admin/sources/bulk`,
  `internal/admin/billing_webhook.go` (daily tenant-usage POST
  to `CONTEXT_ENGINE_BILLING_WEBHOOK_URL`), and four new
  `MessageProbe` implementations on `GET /v1/admin/health/
  summary` (stale connectors, DLQ growth, embedding-model
  availability, Tantivy disk usage). Docs: PROGRESS.md +
  PHASES.md + README.md + ARCHITECTURE.md updated to reflect
  the new 42-connector floor and the Round-18/19 capability
  additions; OpenAPI spec at `docs/openapi.yaml` gains the
  Round-18 endpoints.

- 2026-05-12: **Round 17: Connector catalog expansion — 28 → 36
  production connectors. Tasks 1-8 add Microsoft Entra ID
  (Identity, Graph `$deltatoken`), Google Workspace Directory
  (Identity, `updatedMin` RFC3339 filter), Microsoft 365 Outlook
  (Email, Graph `@odata.deltaLink` mailbox delta), Workday (HR,
  `Updated_From` filter + termination → `ChangeDeleted`),
  BambooHR (HR, basic-auth with api_key/x + `/changed?since`),
  Personio (HR, OAuth client-credentials + `updated_from`),
  sitemap (Generic, `<urlset>` / `<sitemapindex>` recursion + per-
  entry `<lastmod>` cursor), and Coda (Knowledge, `updatedAt`
  DESC walk). Tasks 9-10 lift the connector-completeness audit
  + smoke-test pin to 36. Tasks 11-13 add
  `tests/e2e/round17_test.go`, the Round-17 regression manifest,
  and Round-17 contract assertions (`SourceConnector` +
  `DeltaSyncer` + heterogeneous bootstrap surfaces).
  Task 14 confirms the Round-16 fast-lane gates
  (`fast-connector-integration`, `fast-regression`) cover the new
  surface without further additions. Tasks 15-17 lift the runbook
  gate to 36 + ship 8 new runbooks. Tasks 18-20 refresh
  PROGRESS / README / ARCHITECTURE / PHASES.**
  - **Tasks 1-8**: Eight new connectors live under
    `internal/connector/entra_id/` (Graph delta tokens; disabled
    + `@removed` map to `ChangeDeleted`),
    `internal/connector/google_workspace/` (`updatedMin`
    RFC3339 filter; `suspended=true` maps to `ChangeDeleted`),
    `internal/connector/outlook/` (Graph mailbox delta with
    `@odata.deltaLink` rotation),
    `internal/connector/workday/` (`Updated_From` + termination
    tombstones),
    `internal/connector/bamboohr/` (basic-auth header where
    api_key is the username and `"x"` is the password — pinned
    by `TestBambooHR_BasicAuthHeader`),
    `internal/connector/personio/` (client-credentials grant +
    `updated_from`),
    `internal/connector/sitemap/` (XML decoder that follows
    `<sitemapindex>` shards bounded by `maxDepth` to avoid cycles;
    multi-format `<lastmod>` parser),
    `internal/connector/coda/` (DESC walk with `pageToken`
    pagination). Each connector implements
    `SourceConnector` + `DeltaSyncer`, uses stdlib `net/http`
    with `http.NewRequestWithContext`, wraps HTTP 429 as
    `connector.ErrRateLimited`, and ships with full
    httptest-backed unit tests including the Round-15/16
    bootstrap contract (empty cursor → DESC + `limit=1` →
    "now" cursor without history backfill).
  - **Task 9**: `internal/connector/audit_test.go` audits 35
    first-class connectors (excluding the `google_shared_drives`
    wrapper that delegates to `googledrive`). Every new source
    file references `connector.ErrInvalidConfig`,
    `connector.ErrNotSupported`, `connector.ErrRateLimited`,
    and `http.NewRequestWithContext`.
  - **Task 10**: `tests/e2e/connector_smoke_test.go` pins the
    registry at 36 entries and adds full-lifecycle smoke tests
    (Validate → Connect → ListNamespaces → ListDocuments →
    FetchDocument + DeltaSync) for each of the 8 new connectors.
  - **Task 11**: `tests/e2e/round17_test.go` adds the Round-17
    registry count assertion, Entra ID + BambooHR full
    lifecycles as heterogeneous probes (Graph delta-token vs
    basic-auth + `changed-since`), and a rate-limit sweep that
    probes all 8 new connectors with a 429 fixture and verifies
    `connector.ErrRateLimited` propagation.
  - **Task 12**: `tests/regression/round1617_manifest.go` +
    `_test.go` catalogue the Round-17 fixes (registry expansion
    to 36, DeltaSync bootstrap contract for each new connector,
    deprovisioned-identity → `ChangeDeleted`, 429 → rate-limit
    sentinel sweep, BambooHR basic-auth header order, sitemap-
    index recursion). The meta-test asserts every `TestRef`
    resolves on disk.
  - **Task 13**: `tests/integration/connector_contract_test.go`
    adds compile-time `SourceConnector` + `DeltaSyncer`
    assertions for each of the 8 new structs and a table-driven
    `TestConnectorContract_Round17_DeltaSyncerEmptyCursor`
    covering Graph delta token (Entra ID), `/v1/employees/
    directory` (BambooHR), and sitemap `<lastmod>` (Sitemap)
    bootstrap surfaces.
  - **Task 14**: No CI changes — the existing
    `fast-connector-unit` (`./internal/connector/...`),
    `fast-connector-integration` (`integration`-tagged contract
    tests), and `fast-regression` (manifest meta-tests) lanes
    pick up all Round-17 surfaces automatically. The
    `fast-required` aggregator continues to gate branch
    protection. The `concurrency` group already cancels stale
    runs per PR / per branch.
  - **Tasks 15-17**: `docs/runbooks/runbook_test.go` lifts the
    floor from 28 → 36 and blank-imports all 8 new connectors;
    `docs/runbooks/entraid.md`, `googleworkspace.md`,
    `outlook.md`, `workday.md`, `bamboohr.md`, `personio.md`,
    `sitemap.md`, and `coda.md` each cover the four required
    sections (credential rotation, quota / rate-limit incidents,
    outage detection / recovery, common error codes).
  - **Tasks 18-20**: PROGRESS.md changelog + matrix (this
    expansion), README.md status banner / Round-17 additions
    block / structure tree refresh, ARCHITECTURE.md §9 directory
    tree + "Tech choices added in Round 17" section, PHASES.md
    Phase 7 exit criteria refreshed to 36 connectors.
- 2026-05-12: **Round 16: Connector catalog expansion — 20 → 28
  production connectors. Tasks 1-8 add Mattermost (Chat), ClickUp
  + Monday.com (Issue/project — REST + GraphQL), Pipedrive (CRM),
  Okta (Identity), Gmail (Email read-only via history.list),
  RSS/Atom (Generic feed polling), and Confluence Server/Data
  Center (Knowledge/wiki via CQL `lastModified`). Tasks 9-10
  extend the connector-completeness audit + smoke-test pin to
  28. Tasks 11-13 add `tests/e2e/round16_test.go`, the
  Round-15/16 regression manifest, and Round-16 contract
  assertions. Task 14 wires two new fast-lane CI gates
  (`fast-connector-integration`, `fast-regression`). Tasks 15-17
  pin the runbook gate to 28 entries + 8 new runbooks. Tasks
  18-20 refresh PROGRESS / README / ARCHITECTURE / PHASES.**
  - **Tasks 1-8**: Eight new connectors live under
    `internal/connector/mattermost/`,
    `internal/connector/clickup/`,
    `internal/connector/monday/` (GraphQL; the HTTP-200
    `ComplexityException` envelope wraps to
    `connector.ErrRateLimited`),
    `internal/connector/pipedrive/`,
    `internal/connector/okta/` (RFC 5988 `Link` header
    pagination; deprovisioned status maps to
    `connector.ChangeDeleted`),
    `internal/connector/gmail/` (history-cursor; bootstrap
    fetches `profile.historyId`),
    `internal/connector/rss/` (multi-format timestamp parser),
    `internal/connector/confluence_server/` (PAT or basic auth;
    CQL search). Each connector implements
    `SourceConnector` + `DeltaSyncer`, uses stdlib `net/http`
    only, ships with full httptest unit tests, and is
    blank-imported in `cmd/api/main.go` + `cmd/ingest/main.go`.
  - **Task 9**: `internal/connector/audit_test.go` widened to
    27 audited connectors (excluding the
    `google_shared_drives` registry wrapper).
  - **Task 10**: `tests/e2e/connector_smoke_test.go` registry
    floor moved to 28 and the new full-lifecycle smoke tests
    (mattermost / clickup / monday / pipedrive / okta / gmail /
    rss / confluence_server) wire each connector to a
    httptest-backed mock and run Validate → Connect →
    ListNamespaces → ListDocuments → FetchDocument plus
    DeltaSync.
  - **Task 11**: `tests/e2e/round16_test.go` adds the Round-16
    registry count test, full lifecycle for Gmail + Okta
    (including the DeltaSync bootstrap cursor — the historyId
    monotonic int64 for Gmail, RFC3339 `lastUpdated` for Okta),
    and a 7-source `TestRound16_NewConnectors_RateLimitedOnListDocuments`
    table that asserts every new connector propagates
    `ErrRateLimited` from `ListDocuments`.
  - **Task 12**: `tests/regression/round1516_manifest.go` +
    `_test.go` catalogue 6 Round-15/16 fixes (registry
    expansion, DeltaSync bootstrap pattern,
    `ComplexityException` envelope, Gmail history bootstrap,
    Okta deprovisioned `ChangeDeleted`, audit-coverage widening)
    and the meta-test asserts every TestRef resolves on disk.
  - **Task 13**: `tests/integration/connector_contract_test.go`
    grew compile-time `SourceConnector` + `DeltaSyncer`
    assertions per new connector struct and a new table-driven
    `TestConnectorContract_Round16_DeltaSyncerEmptyCursor`
    against Gmail + Okta (heterogeneous bootstrap surfaces).
  - **Task 14**: Two new fast-lane CI jobs in
    `.github/workflows/ci.yml` —
    `fast-connector-integration` (runs
    `go test -tags=integration ./tests/integration/... -run
    TestConnectorContract` so docker-compose-backed integration
    tests do not gate PRs) and `fast-regression` (runs
    `go test -count=1 ./tests/regression/...`). Both are wired
    into the `fast-required` aggregator.
  - **Task 15**: `docs/runbooks/runbook_test.go` registry floor
    lifted from 20 → 28 and the blank-imports widened to all
    new connector packages.
  - **Tasks 16-17**: 8 new per-connector runbooks under
    `docs/runbooks/` (mattermost.md, clickup.md, monday.md,
    pipedrive.md, okta.md, gmail.md, rss.md,
    confluenceserver.md) each carrying the 4 required sections
    enforced by `runbook_test.go`. The default
    `runbookFilename` mapping (`strings.ReplaceAll("_", "")`)
    already covers `confluence_server` → `confluenceserver.md`.
  - **Tasks 18-20**: PROGRESS.md (this entry + matrix
    expansion), README.md status banner / Round-16 additions
    block / project structure tree, ARCHITECTURE.md §9 + new
    "Tech choices added in Round 16" section, PHASES.md Phase
    7 exit criteria refreshed to 28 connectors.
- 2026-05-12: **Round 15: Connector catalog expansion — 12 → 20
  production connectors. Tasks 1-8 add KChat (the missing
  Phase-1 chat connector), S3-compatible object storage, Linear,
  Asana, Discord, Salesforce, HubSpot, and a Google Shared
  Drives registry entry that filters out My Drive. Tasks 9-10
  add a connector-completeness audit + smoke-test pin at 20.
  Tasks 11-13 add round15 e2e, round1415 regression manifest,
  and an integration contract test. Tasks 14-15 add a
  `fast-connector-unit` CI lane + `concurrency` group. Tasks
  16-17 add 7 new runbooks + OpenAPI sweep. Tasks 18-20 refresh
  PROGRESS / README / ARCHITECTURE / PHASES.**
  - **Task 0 (CI audit)**: `.github/workflows/ci.yml` now has a
    `concurrency` group that cancels in-progress runs when a
    new commit lands on the same PR. The `fast-required`
    aggregator gained `fast-connector-unit` in its `needs:`
    list so the connector-only lane gates the same surface.
  - **Tasks 1-7**: Eight new connectors live under
    `internal/connector/kchat/`,
    `internal/connector/s3/` (with `sigv4.go` for AWS-style
    signing — no vendor SDK),
    `internal/connector/linear/` (GraphQL),
    `internal/connector/asana/`,
    `internal/connector/discord/`,
    `internal/connector/salesforce/` (SOQL),
    `internal/connector/hubspot/` (CRM v3). Each implements
    `SourceConnector` + `DeltaSyncer` (and `WebhookReceiver`
    where the source supports it) with stdlib `net/http` and
    httptest-backed unit tests.
  - **Task 8**: `internal/connector/googledrive/shared_drives.go`
    wraps the existing connector and registers
    `google_shared_drives` — its `ListNamespaces` filters out
    My Drive so the registry exposes the shared-drive surface
    as a separate ingestion entity. `ListNamespaces` also now
    paginates via `nextPageToken` so workspaces with >100
    shared drives backfill in full.
  - **Task 9**: `internal/connector/source_connector.go` adds
    a new `ErrRateLimited` sentinel. Every connector iterator
    (existing + new) wraps HTTP 429 (and Slack's
    `ok=false, error=ratelimited` 200 variant) with
    `fmt.Errorf("%w: …", connector.ErrRateLimited, …)` so the
    adaptive rate limiter in `internal/connector/adaptive_rate.go`
    can react. `internal/connector/audit_test.go` is a
    process-global gate that fails CI if any connector source
    drops the wrap.
  - **Task 10**: `tests/e2e/connector_smoke_test.go` asserts
    the registry has exactly 20 entries (was 12) and adds
    per-connector smoke tests for the 8 new entries.
  - **Tasks 11-13**: `tests/e2e/round15_test.go` covers
    registry count + a full KChat lifecycle + 429 propagation
    through iterators. `tests/regression/round1415_manifest.go`
    + `_test.go` catalogue the 5 Round-14/15 fixes; the
    meta-test asserts every TestRef resolves on disk.
    `tests/integration/connector_contract_test.go` (build tag
    `integration`) is the compile-time + behaviour-time
    contract: interface assertions for every connector struct,
    empty-cursor semantics for DeltaSyncer, panic-safety for
    WebhookReceiver.
  - **Tasks 14-15**: `fast-connector-unit` job runs `time go
    test -race -count=1 ./internal/connector/...` in isolation
    so connector-only PRs get sub-30s feedback. Concurrency
    group `ci-${{ github.workflow }}-${{
    github.event.pull_request.number || github.sha }}` cancels
    stale runs. Existing fast-lane jobs already share `setup-go`
    cache + `actions/cache` build-cache keys (Round-14 Task
    16) — no additional changes needed.
  - **Tasks 16-17**: `docs/runbooks/{kchat,s3,linear,asana,
    discord,salesforce,hubspot}.md` — seven new runbooks each
    covering credential rotation, quota / rate-limit incidents,
    outage detection / recovery, and the common error-code
    surface (enforced by `docs/runbooks/runbook_test.go`).
    `google_shared_drives` reuses the existing
    `docs/runbooks/googledrive.md`. OpenAPI surface unchanged
    by Round 15 (no new HTTP endpoints).
  - **Tasks 18-20**: This Changelog entry, capability matrix,
    README banner, `ARCHITECTURE.md` §9 directory tree, and
    `PHASES.md` Phase-7 status all updated to reflect the
    20-connector catalog. Stale "12 connectors" references
    swept.
- 2026-05-12: **Round 14: Next 20 tasks — observability dashboards
  (stage breakers, latency histogram, slow-query persistence,
  pipeline throughput), payload schema validation, audit
  integrity worker, API-key grace sweeper, per-tenant payload
  limits, regression manifest, e2e + fuzz testing, cache
  invalidation audit refresh, embedding fallback metrics, DLQ
  categorisation, CI parallel split + caching, OpenAPI Round-13
  schemas, four new Prometheus alerts, doc audit**.
  - **Task 0 (CI)**: `docs/PROGRESS.md` already records the
    `DLQAgeHigh` alert at `severity: warning` matching
    `deploy/alerts.yaml` — confirmed in commit `94034c7`.
  - **Task 1**: `internal/admin/stage_breaker_handler.go`
    serves `GET /v1/admin/pipeline/breakers` with per-stage
    `StageBreakerSnapshot` rows (state, fail_count, opened_at,
    probe_in_flight, threshold, open_for). Backed by a new
    `Snapshot()` method on `StageCircuitBreaker` + an
    in-process `StageBreakerRegistry` so the admin handler
    reads state without coupling to the producer. Test in
    `internal/admin/stage_breaker_handler_test.go`. OpenAPI
    pinned.
  - **Task 2**: `internal/admin/latency_histogram_handler.go`
    serves `GET /v1/admin/retrieval/latency-histogram` returning
    P50/P75/P90/P95/P99 per backend (vector / bm25 / graph /
    memory / merge / rerank / total) over a configurable
    rolling window. Implementation is an in-process 60-bucket
    1-minute ring buffer feeding nearest-rank percentile maths;
    no Prometheus query path on the hot retrieve loop. Test
    asserts percentile shape across 1000 deterministic samples.
  - **Task 3**: `migrations/038_slow_queries.sql` adds the
    `slow_queries` table with `(tenant_id, created_at DESC)` +
    `(tenant_id, latency_ms DESC)` indexes; rollback in
    `migrations/rollback/038_slow_queries.down.sql`.
    `internal/admin/slow_query_store.go` adds a GORM store +
    SQLite-backed test. `internal/admin/slow_query_log_handler.go`
    serves `GET /v1/admin/retrieval/slow-queries?since=&limit=`.
  - **Task 4**: `internal/admin/pipeline_throughput_handler.go`
    + `internal/admin/pipeline_throughput_recorder.go` track
    per-stage event counts and average latency in a 60-bucket
    1-minute ring buffer. `GET /v1/admin/pipeline/throughput?
    window=5m` returns the rolled-up totals. OpenAPI pinned.
  - **Task 5**: `internal/retrieval/payload_validator.go` runs
    the two-layer payload check (JSON well-formed → struct
    field validation) on `/v1/retrieve`, `/v1/retrieve/batch`,
    and `/v1/retrieve/stream`. Rejection emits structured
    `ERR_INVALID_PAYLOAD` (new entry in
    `internal/errors/catalog.go`). Test covers malformed JSON,
    empty query, oversize top-k.
  - **Task 6**: `internal/audit/integrity_worker.go` adds a
    background hash-chain verification loop, gated on
    `CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK=true` and configurable
    via `CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK_INTERVAL`. On a
    detected break it emits an `audit.integrity_violation`
    audit event and invokes a `ObserveFn` callback so
    `cmd/api/main.go` can increment
    `context_engine_audit_integrity_violations_total` without
    creating an `audit → observability` import cycle. Test
    exercises pass and fail paths.
  - **Task 7**: `internal/admin/api_key_sweeper.go` sweeps
    grace-period keys past their `grace_until` deadline and
    transitions them to `expired`. Counters:
    `context_engine_api_keys_expired_total`. Gauge:
    `context_engine_api_keys_grace_expiring_soon` for the new
    APIKeyGraceExpiringSoon alert. Time-controlled clock in
    the test.
  - **Task 8**: `migrations/039_tenant_payload_limits.sql` adds
    a per-tenant payload-cap table. `internal/admin/tenant_pay
    load_limits.go` implements the GORM store + cache + a
    `TenantPayloadLookup` adapter that plugs into the existing
    payload limiter's `TenantOverride` callback. SQLite-backed
    CRUD test + middleware integration test.
  - **Task 9**: `tests/regression/round1213_manifest.go`
    catalogues six PR #22 review fixes (stage breaker probe
    gate, SLO burn-rate multi-window fix, BackendContributions
    in runPipelineFromVec, API-key atomic Rotate, stage breaker
    early-exit release, batch trace_id echo). Meta-test
    verifies every TestRef exists in the tree.
  - **Task 10**: `tests/e2e/round13_test.go` (build tag `e2e`)
    covers slow-query log, API-key rotation → grace → sweep,
    pipeline throughput endpoint, stage breaker dashboard, and
    payload size limiter 413.
  - **Task 11**: Four fuzz targets:
    `FuzzAPIKeyRowDecode` (admin), `FuzzStageBreakerConcurrent`
    (pipeline), `FuzzHealthSummaryRequest` (admin),
    `FuzzSlowQueryThreshold` (retrieval). Added to `make fuzz`.
  - **Task 12**: `internal/retrieval/cache_invalidation_test.go`
    re-audited through Round 13; the baseline manifest is
    unchanged because no Round-9..13 task added a new
    cache-affecting write path. Comment block now reflects the
    audit window so the next round knows where to start.
  - **Task 13**: `internal/pipeline/embed_fallback.go` exports
    `EmbedWithReason(reason)` and instruments the fallback path
    with `context_engine_embedding_fallback_total{reason}` +
    `context_engine_embedding_fallback_latency_seconds`. Test
    in `internal/pipeline/embed_fallback_metrics_test.go`.
  - **Task 14**: `migrations/040_dlq_category.sql` adds a
    `category` column to `dlq_messages` (transient | permanent
    | unknown). `CategoriseDLQError` populates it at insert
    time; the auto-replayer skips `permanent` rows and the
    admin list endpoint exposes a `?category=` filter. Test
    asserts skip behaviour + categorisation table.
  - **Task 15**: `.github/workflows/ci.yml` splits the legacy
    `fast-go` job into `fast-check` (gofmt + vet),
    `fast-test` (race + cover), and `fast-build` (cmd binaries)
    so the long-pole test step no longer serialises the build.
    The `fast-required` aggregator's `needs:` list is updated.
  - **Task 16**: Each new fast-lane Go job restores
    `~/.cache/go-build` via `actions/cache` keyed on
    `go.sum`. The `full-e2e` job sets up Docker Buildx so
    `docker compose up` reuses cached image layers.
  - **Task 17**: `docs/openapi.yaml` gains typed schemas for
    `/v1/admin/pipeline/breakers`,
    `/v1/admin/retrieval/latency-histogram`,
    `/v1/admin/retrieval/slow-queries`, and
    `/v1/admin/pipeline/throughput`. The Round-13 surface
    (health summary, slow queries, cache stats, API-key
    rotation, audit integrity) is now pinned by
    `docs/openapi_test.go`.
  - **Task 18**: `deploy/alerts.yaml` gains four alerts:
    `AuditIntegrityViolation` (page),
    `EmbeddingFallbackRateHigh` (warning, > 10% over 15m),
    `APIKeyGraceExpiringSoon` (warning), and
    `SlowQueryRateHigh` (warning, > 5% over 15m). Each is
    backed by a metric registered in
    `internal/observability/metrics.go`. `make alerts-check`
    is green.
  - **Tasks 19-20**: This changelog entry, plus a refresh of
    `README.md` / `docs/ARCHITECTURE.md` / `docs/PHASES.md`
    Round-14 banners. Migration count now reads 040.

- 2026-05-11: **Round 13: Next 20 tasks — health summary, SLO
  burn-rate alerts, batch tracing, DLQ age monitor, stage
  breakers, percent_complete, backend contributions, slow-query
  log, cache-stats, API-key rotation, payload limiter, audit
  integrity chain, chaos kafka + concurrent-delete e2e, eval
  corpus 50, `make doctor`, OpenAPI completeness gate, PG pool
  leak detector, embed fallback, Postgres migrate-dry-run**.
  - **Task 0 (CI)**: `.github/workflows/ci.yml` splits `fast-go`
    so `golangci-lint` runs in its own `fast-lint` job in
    parallel with gofmt+vet+race+cover+build. Brings fast-lane
    wall-clock under 3 minutes.
  - **Task 1**: `internal/admin/health_summary_handler.go`
    serves `GET /v1/admin/health/summary` and fans out to every
    health probe (Postgres, Redis, Qdrant, Kafka, gRPC sidecars,
    credential health) in parallel. Returns a verdict
    (healthy / degraded / unhealthy) plus per-component latency
    + error. Test in
    `internal/admin/health_summary_handler_test.go`. Documented
    in `docs/openapi.yaml`.
  - **Task 2**: `deploy/alerts/slo_burn_rate.yaml` declares
    multi-window burn-rate alerts for the retrieval P95 SLO
    (500 ms) and the pipeline throughput SLO, both keyed off
    the existing recording rules. Wired into `make alerts-check`
    and asserted in `deploy/alerts_test.go`.
  - **Task 3**: `internal/retrieval/batch_handler.go` wraps the
    fan-out in a parent OTel span; sub-requests are emitted as
    children. The batch response now returns the parent
    `trace_id`. Test exercises the linkage.
  - **Task 4**: `cmd/ingest/main.go` runs a `DLQAgeMonitor`
    goroutine that publishes
    `context_engine_dlq_oldest_message_age_seconds`. New
    `DLQAgeHigh` alert in `deploy/alerts.yaml` (severity
    `warning`, fires when oldest > 1 h). Tests in
    `internal/pipeline/dlq_age_monitor_test.go`.
  - **Task 5**: `internal/pipeline/stage_breaker.go` adds
    per-stage circuit breakers; consecutive failures on Parse /
    Embed short-circuit events to the DLQ. Gated on
    `CONTEXT_ENGINE_STAGE_BREAKER_ENABLED`. Metrics:
    `context_engine_pipeline_stage_breaker_transitions_total`
    and `_short_circuits_total`. Tests with fault injection.
  - **Task 6**: `internal/admin/sync_progress_handler.go` now
    aggregates per-namespace progress into a source-level
    `percent_complete` weighted by discovered-doc count.
  - **Task 7**: `internal/retrieval/explain.go` returns a
    `backend_contributions` map (vector / BM25 / graph / memory)
    showing how many results each backend contributed to the
    final top-K after RRF merge.
  - **Task 8**: `internal/admin/slow_query.go` records retrievals
    exceeding `CONTEXT_ENGINE_SLOW_QUERY_THRESHOLD_MS` (default
    1000 ms) into `query_analytics` with `slow=true`. New
    handler at `GET /v1/admin/analytics/queries/slow`.
  - **Task 9**: `internal/admin/cache_stats_handler.go` exposes
    `GET /v1/admin/analytics/cache-stats` returning per-tenant
    cache hits / misses / hit_rate_pct over a configurable
    window. Backed by the existing
    `context_engine_retrieval_cache_*` counters.
  - **Task 10**: `internal/admin/api_key_rotation.go` adds
    `POST /v1/admin/tenants/:tenant_id/rotate-api-key`. New key
    returned exactly once; old key remains valid for
    `CONTEXT_ENGINE_API_KEY_GRACE_PERIOD` (default 24 h).
    Migration `036_api_keys.sql` if not already present. Audit
    event `api_key.rotated`.
  - **Task 11**: `internal/observability/payload_limiter.go`
    rejects requests larger than
    `CONTEXT_ENGINE_MAX_REQUEST_BODY_BYTES` (default 10 MiB)
    with HTTP 413. Wired into both `cmd/api` and `cmd/ingest`
    probe servers. `internal/observability/payload_limiter_test.go`
    covers bypass paths + oversized bodies.
  - **Task 12**: `internal/audit/integrity.go` implements
    `ComputeIntegrity()` — a deterministic SHA-256 hash chain
    over audit rows sorted oldest-first. New
    `GET /v1/admin/audit/integrity` returns head hash + entry
    count so operators can detect tampering. Test exercises the
    empty / append / tamper / delete cases.
  - **Task 13**: `tests/e2e/chaos_kafka_test.go` (build tag
    `e2e`) simulates a Kafka broker outage via an in-process
    `chaosEmitter`. Twenty concurrent events retry / fall back
    to DLQ as the broker recovers; the test asserts
    successes + DLQ'd = 20.
  - **Task 14**: `tests/e2e/concurrent_delete_test.go` (build
    tag `e2e`) spawns two goroutines that simultaneously call
    `shard.Forget()` against the same tenant. The fenced Redis
    lease is the single race point — exactly one succeeds and
    the other observes `shard.ErrLeaseHeld`.
  - **Task 15**: `tests/eval/golden_corpus.json` expanded from
    20 to 50 cases covering multi-hop graph, BM25 exact-match,
    memory-augmented, and cross-namespace queries. Thresholds
    (Precision@5 ≥ 0.6, Recall@5 ≥ 0.6, MRR ≥ 0.7, nDCG ≥ 0.7)
    held without adjustment.
  - **Task 16**: `make doctor` (via `scripts/doctor.sh`) checks
    Go ≥ 1.25, Docker daemon, docker-compose, Python 3.11+,
    protoc, golangci-lint, and the e2e env vars; prints a
    green / red checklist and exits non-zero on hard failure.
  - **Task 17**: `docs/openapi_test.go` adds
    `TestOpenAPI_RouterCoverage` — an AST walk over
    `internal/admin`, `internal/audit`, `internal/retrieval`
    that enumerates every gin route registration prefixed with
    `/v1/` and asserts each has a matching path entry in
    `docs/openapi.yaml`. Catches new endpoints shipped without
    documentation. Runs in the fast lane.
  - **Task 18**: `internal/observability/pool_leak_detector.go`
    periodically reads `db.Stats().OpenConnections` against
    `CONTEXT_ENGINE_PG_MAX_OPEN` and logs a structured warning
    when utilisation stays above 90 % for three consecutive
    samples. Wired into `cmd/api/main.go`; publishes
    `context_engine_postgres_pool_utilization_percent`.
  - **Task 19**: `internal/pipeline/embed_fallback.go` provides
    a deterministic Go-native 256-dim hashing-trick embedder
    that activates when the gRPC sidecar circuit breaker opens
    and `CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED` is set. Stored
    chunks carry `degraded_embedding=true` so retrieval can
    filter / down-weight them.
  - **Task 20**: `scripts/migrate-dry-run-pg.sh` + new
    `make migrate-dry-run-pg` target launch a disposable
    Postgres 16 container, apply every `migrations/*.sql` in
    order, then run the matching rollbacks. Wired into the
    full-lane CI job `full-migrate-dry-run-pg`. Catches
    Postgres-specific syntax errors (JSONB, TIMESTAMPTZ,
    `ADD COLUMN IF NOT EXISTS`) the SQLite-based dry-run
    cannot.

- 2026-05-11: **Round 12: Next 20 tasks — observability alerts,
  resilience hardening, CI gates, OpenAPI completeness, fuzz
  expansion, docs audit**.
  - **Task 1**: `deploy/alerts.yaml` declares
    `GRPCCircuitBreakerOpen` (fires when
    `context_engine_grpc_circuit_breaker_state{target} == 2` for
    >5m, severity `page`). Test in `deploy/alerts_test.go`.
  - **Task 2**: `PostgresPoolSaturated` and `RedisPoolSaturated`
    alerts in `deploy/alerts.yaml` watch pool utilisation
    relative to the configured max; tests assert presence and
    thresholds.
  - **Task 3**: `internal/pipeline/retention_worker.go` records
    `context_engine_retention_expired_chunks_total` (counter) and
    `context_engine_retention_sweep_duration_seconds` (histogram)
    on every sweep. Registered + `ResetForTest`-covered in
    `internal/observability/metrics.go`. Tests in
    `internal/pipeline/retention_worker_metrics_test.go`.
  - **Task 4**: `internal/admin/scheduler.go` wraps the tick in a
    `recover()` (`SafeTick`) so a panic in an emit path cannot
    kill the scheduler goroutine. `context_engine_scheduler_errors_total`
    counts recovered panics + propagated errors; `last_error` /
    `last_error_at` persisted on the `sync_schedules` row.
    Tests in `internal/admin/scheduler_resilience_test.go`.
  - **Task 5**: Python sidecars (`services/docling`,
    `services/embedding`, `services/memory`) register the
    gRPC health protocol (`grpc_health.v1`). Each service has a
    pytest covering `Check() == SERVING`. Documented in
    `docs/ARCHITECTURE.md` §2.3.
  - **Task 6**: `internal/pipeline/dlq_auto_replay.go` scans
    `dlq_messages` for rows with `replay_count < max_auto_retries`
    and `next_replay_at <= now()`; re-emits via the existing
    Replayer with capped backoff (1m / 5m / 30m). Gated on
    `CONTEXT_ENGINE_DLQ_AUTO_REPLAY=true`. Metric:
    `context_engine_dlq_auto_replays_total`. Test in
    `internal/pipeline/dlq_auto_replay_test.go`.
  - **Task 7**: `tests/e2e/isolation_smoke_test.go` ingests one
    document per tenant for two tenants and asserts
    `POST /v1/admin/isolation-check` returns a clean report.
    `make test-isolation` target wires this into CI's full lane.
  - **Task 8**: `make eval` runs the golden-corpus eval and fails
    if Precision@5 < 0.8. Added to CI fast lane.
  - **Task 9**: `make migrate-dry-run` runs the migrate runner
    with `DryRun=true` against a fresh SQLite database. Wired
    into CI fast lane.
  - **Task 10**: `docs/openapi.yaml` adds 34 typed response
    schemas covering retrieval feedback, eval results, sync
    schedule, models catalog, credential health, export jobs,
    A/B experiments, warm-cache, bulk sources, chunk quality,
    latency budget, cache config, sync history, pinned results,
    connector templates, synonyms, notification preferences.
    24 endpoint responses converted from
    `additionalProperties: true` / `description: OK` stubs to
    typed schema refs.
  - **Task 11**: Typed `requestBody` schemas for the core
    endpoints — `BatchRetrieveRequest`, `StreamRetrieveRequest`,
    `CreateSourceRequest`, `PolicySimulateRequest` — mirror the
    Go handler's `ShouldBindJSON` targets and replace untyped
    bodies in `/v1/retrieve/batch`, `/v1/retrieve/stream`,
    `/v1/admin/sources`, `/v1/admin/policy/simulate`.
  - **Task 12**: New Go-native fuzz targets:
    `internal/pipeline/consumer_fuzz_test.go::FuzzParsePartitionKey`,
    `internal/policy/privacy_mode_fuzz_test.go::FuzzEffectiveMode`,
    `internal/shard/delta_fuzz_test.go::FuzzDeltaDiff`,
    `internal/admin/query_analytics_fuzz_test.go::FuzzQueryHash`.
    `Makefile` `fuzz` target lists each target individually.
  - **Task 13**: `internal/connector/adaptive_rate.go` sets the
    `context_engine_adaptive_rate_current{connector}` gauge on
    every rate change and increments
    `context_engine_adaptive_rate_halved_total{connector}` on
    every halve. Test in
    `internal/connector/adaptive_rate_metrics_test.go`.
  - **Task 14**: `internal/retrieval/cache_invalidation_test.go`
    uses Go AST analysis to verify every Stage-4 write
    (`QdrantClient.Upsert`, `FalkorDBClient.WriteNodes`,
    `BleveClient.Index`) inside `internal/pipeline/` is paired
    with a `cache.Invalidate` call in the same function or its
    immediate caller. Catches new write paths that ship without
    a cache invalidation companion.
  - **Task 15**: `tests/integration/rbac_coverage_test.go`
    (build tag `integration`) enumerates every route under the
    `/v1/admin/` group and asserts each has the RBAC middleware
    in its handler chain. Catches new admin handlers that ship
    without role gating.
  - **Task 16**: `internal/admin/rate_limit_status_handler.go`
    exposes `GET /v1/admin/sources/:id/rate-limit-status`
    returning `current_tokens`, `max_tokens`, `effective_rate`,
    `halve_count`, `last_429_at`, `is_throttled`. Built on the
    `RateLimitInspector` interface for test hermeticity. Tests
    use miniredis. Documented in `docs/openapi.yaml`.
  - **Task 17**: `internal/admin/audit_retention.go` background
    sweeper deletes audit_logs rows older than
    `CONTEXT_ENGINE_AUDIT_RETENTION_DAYS` (default 90) in
    batches of 1000 rows. Metric
    `context_engine_audit_rows_expired_total`. Migration
    `migrations/034_audit_retention.sql` adds an index on
    `audit_logs.created_at` if missing.
  - **Task 18**: `tests/regression/round911_manifest.go` +
    `_test.go` catalogue the six Round-11 Devin Review fixes
    (batch topK fallback, cache-warm analytics tagging, stream
    explain auth, backend_timings schema alignment, hook panic
    recovery, QueryAnalyticsSource constants). Meta-tests assert
    every named regression test exists in the tree.
  - **Task 19**: `tests/e2e/round12_test.go` (build tag `e2e`)
    smoke-tests the Round-12 surface as a single bundle:
    DLQ auto-replay, rate-limit status, audit retention,
    isolation across two tenants, scheduler panic recovery, and
    Round-12 alert rule presence.
  - **Task 20**: PROGRESS / README / ARCHITECTURE / PHASES
    audit — Round 12 changelog (this entry), README "Round 12
    additions" block, ARCHITECTURE "Tech choices added in
    Round 12" section, PHASES Round 12 status note,
    `test-isolation` and `migrate-dry-run` added to the Make
    targets table, connector count + migration count
    cross-checked (12 connectors, migrations through 034).

- 2026-05-11: **Round 11: Next 20 tasks — fuzz fix, e2e depth,
  hook observability, batch diversity, graceful degradation, docs
  audit**.
  - **Task 1**: `Makefile` `fuzz` target enumerates each fuzz
    target individually (one `go test -fuzz='^FuzzName$'` per
    invocation) so Go's "fuzz pattern must match exactly one
    target" rule never fires on the nightly job. Validated by
    `make fuzz`. (Carryover from commit a0cf6229.)
  - **Task 2**: `tests/e2e/round10_test.go` (build tag `e2e`)
    exercises the five Round-10 wiring hooks
    (`LatencyBudgetLookup`, `CacheTTLLookup`,
    `SyncHistoryRecorder`, `ChunkScorer`, `QueryExpander`)
    end-to-end and asserts tenant isolation.
  - **Task 3**: `tests/regression/round910_manifest.go` +
    `_test.go` catalogue the Round 9/10 Devin Review fixes
    (fuzz multi-match, OpenAPI GET→POST for isolation-check,
    scheduler lifecycle drain). Mirrors the Round-7/8 manifest
    pattern; meta-tests assert every named regression test
    exists.
  - **Task 4**: `internal/pipeline/chunk_quality_hook.go` now
    increments `context_engine_chunk_quality_errors_total` and
    emits a structured `slog.Warn` on every recorder failure;
    test in `internal/pipeline/round10_hooks_test.go`.
  - **Task 5**: Stage-4 hot-path GORM hooks
    (`scoreAndRecordBlocks`, `recordSyncStart`,
    `FinishBackfillRun`) wrapped in `runWithHookTimeout`
    (configurable via `CONTEXT_ENGINE_HOOK_TIMEOUT`, default
    500ms). New `HookTimeoutsTotal{hook}` counter.
  - **Task 6**: `internal/retrieval/batch_handler.go` propagates
    `Diversity` (lambda) from the batch request to every
    sub-request. Test
    `TestBatch_DiversityFieldThreadedToSubRequests`.
  - **Task 7**: `internal/retrieval/stream_handler.go` threads
    the per-backend explain trace through every SSE event when
    the caller sets `explain: true`. Test
    `TestStreamHandler_ExplainTraceEmitted`.
  - **Task 8**: `internal/shard/generator.go` filters
    pre-generated chunks by the policy snapshot's chunk-ACL
    after the policy gate; chunks carry a `Tags` field that
    the ACL evaluator consumes. Test
    `TestGenerator_FiltersByChunkACL`.
  - **Task 9**: `internal/admin/query_analytics.go` adds a
    `source` enum field (`user` | `cache_warm` | `batch`);
    migration `033_query_analytics_source.sql` (+ rollback).
    The retrieval handler tags every event with the correct
    source per call site.
  - **Task 10**: `cmd/api/readyz.go` enriches the response body
    with `latencies` map (`postgres_ms`, `redis_ms`,
    `qdrant_ms`) so operators can distinguish "up but slow"
    from "up and healthy". Test
    `TestApiReadyz_LatencyFieldsPresent`.
  - **Task 11**: `deploy/alerts.yaml` gains four new alerts
    (`ChunkQualityScoreDropped`, `CacheHitRateLow`,
    `CredentialHealthDegraded`, `GORMStoreLatencyHigh`) plus
    four supporting metrics
    (`context_engine_chunk_quality_score_avg`,
    `context_engine_cache_hit_rate`,
    `context_engine_credential_invalid_sources`,
    `context_engine_gorm_query_duration_seconds`). Validated
    by `make alerts-check`.
  - **Task 12**: cardinality policy documented at the top of
    `internal/observability/metrics.go`: no metric may use
    `tenant_id` as a label. Enforced by
    `TestMetrics_NoTenantIDLabel`.
  - **Task 13**: `internal/errors/catalog.go` adds 10 admin
    error codes (`CodeMissingTenant`, `CodeInvalidRequestBody`,
    `CodeChunkQualityFailed`, …) with HTTP status mappings;
    new test `TestCatalog_AllCodesUniqueAndValid`.
  - **Task 14**: `migrations/migration_order_test.go` enforces
    three structural invariants on the migrations directory:
    no duplicate numeric prefixes, monotonic numbering from
    `001`, every forward migration has a matching
    `rollback/NNN_*.down.sql`.
  - **Task 15**: `tests/integration/audit_completeness_test.go`
    (build tag `integration`) catalogues every mutable admin
    endpoint with its expected `audit.Action`; meta-tests
    enforce coverage and that every documented action
    constant resolves.
  - **Task 16**: `docs/CUTOVER.md` gains a "Round 6–11
    additions" section documenting the new feature flags,
    rollback procedure, capacity test, cardinality policy,
    and alert rules.
  - **Task 17**: `docs/openapi.yaml` replaces
    `additionalProperties: true` stubs with typed schemas
    for `DashboardResponse`, `HealthResponse`, `Capabilities`,
    `PipelineHealthReport`, `AuditListResponse`,
    `DLQListResponse`, `PolicyDraft`, `PolicyDraftList`,
    `SourceList`. Endpoints
    `/v1/health`, `/v1/admin/audit`, `/v1/admin/sources`,
    `/v1/admin/dlq`, `/v1/admin/policy/drafts`,
    `/v1/admin/pipeline/health` now reference the typed
    schemas in their `200` response.
  - **Task 18**: `internal/retrieval/graceful_degradation.go`
    wraps `LatencyBudgetLookup`, `CacheTTLLookup`, and
    `PinLookup` calls in a 200ms timeout + panic-recovery
    wrapper. Failures increment
    `context_engine_gorm_store_lookup_errors_total{store}`
    and emit a `slog.Warn` with `tenant_id`. The retrieval
    handler degrades to defaults rather than 500. Tests in
    `internal/retrieval/graceful_degradation_test.go`.
  - **Task 19**: `tests/e2e/round11_test.go` (build tag `e2e`)
    smoke-tests batch diversity, SSE explain, query analytics
    source, migration ordering, and the graceful-degradation
    path under a hanging budget store.
  - **Task 20**: this doc + `README.md` + `ARCHITECTURE.md` +
    `PHASES.md` refreshed for the Round-11 additions.

- 2026-05-11: **Round 10: Next 20 tasks — wiring + e2e for the
  Round-7..9 stores, fuzz expansion, OpenAPI / runbook gates, CI
  lane audit, and doc refresh**.
  - **Task 1**: `internal/admin/latency_budget.go` is now wired
    into the retrieval handler through `LatencyBudgetLookup`. The
    handler resolves the tenant's `max_latency_ms` from the GORM
    store and seeds the per-request deadline when the caller
    omits `limits.max_latency_ms`. Test in
    `internal/retrieval/round10_handler_test.go`.
  - **Task 2**: `internal/admin/cache_config.go` is wired into
    `internal/storage/redis_cache.go` via `CacheTTLLookup`. `Set`
    consults the per-tenant TTL row and falls back to the global
    default when no row exists. Test in
    `internal/storage/redis_cache_tenantttl_test.go`.
  - **Task 3**: `pipeline.Coordinator` now records sync history
    on every backfill kickoff (`SyncHistoryRecorder` port), and
    `cmd/ingest/main.go` wires it through the GORM
    `SyncHistoryStore`. Tests in
    `internal/pipeline/round10_hooks_test.go` and
    `internal/pipeline/sync_history_hook.go`.
  - **Task 4**: `pipeline.ChunkScorer` runs as a Stage-4
    pre-write hook (gated by
    `CONTEXT_ENGINE_CHUNK_SCORING_ENABLED`); scored chunks land
    in `internal/admin/chunk_quality_gorm.go`. Tests in
    `internal/pipeline/chunk_quality_hook.go` +
    `round10_hooks_test.go`. `cmd/ingest/main.go` owns the
    wiring.
  - **Task 5**: `internal/admin/credential_health.go` runs as a
    periodic goroutine in `cmd/api/main.go`; interval is
    `CONTEXT_ENGINE_CREDENTIAL_CHECK_INTERVAL` (default 1h) and
    the worker is registered on the lifecycle shutdown runner.
  - **Task 6**: `internal/admin/token_refresh.go` ships as a
    periodic goroutine in `cmd/api/main.go`; interval is
    `CONTEXT_ENGINE_TOKEN_REFRESH_INTERVAL` (default 15m) and is
    likewise on the lifecycle runner.
  - **Task 7**: `internal/admin/retention_worker.go` is wired
    into `cmd/ingest/main.go` behind
    `CONTEXT_ENGINE_RETENTION_ENABLED` (or the legacy
    `CONTEXT_ENGINE_RETENTION_INTERVAL`) and sweeps expired
    chunks per tenant.
  - **Task 8**: `internal/admin/scheduler.go` is wired into
    `cmd/ingest/main.go` behind
    `CONTEXT_ENGINE_SCHEDULER_ENABLED` (default on); the
    scheduler emits Kafka ingest events on cron and shuts down
    via the lifecycle runner.
  - **Task 9**: end-to-end integration test
    `tests/integration/query_expansion_test.go` (build tag
    `integration`) seeds synonyms via the GORM store and asserts
    the BM25 backend receives the expanded query. Handler wiring
    via the new `SetQueryExpander` setter in
    `internal/retrieval/handler_setters.go`.
  - **Task 10**: adaptive rate-limit coverage audit: the existing
    `internal/connector/adaptive_rate_test.go` already pins the
    halve-on-429 / climb-on-success / floor-clamp behaviour; no
    additional production code change.
  - **Task 11**: `tests/e2e/round9_test.go` (build tag `e2e`)
    exercises the five Round-9 GORM-backed admin surfaces
    (notifications, A/B tests, connector templates, synonyms,
    chunk quality) and pins tenant isolation on each.
  - **Task 12**: `tests/e2e/round9_pipeline_test.go` (build tag
    `e2e`) drives `CONTEXT_ENGINE_PARSE_TIMEOUT=1ms` against a
    slow parse stage and asserts the timeout fires per-retry; a
    second subtest pins the `CacheWarmOnMiss=true` path —
    response returns before the cache `Set` finishes.
  - **Task 13**: `migrations/032_synonyms.sql` +
    `migrations/rollback/032_synonyms.down.sql` confirmed to
    exist; rollback parity gated by
    `migrations/rollback/rollback_test.go`.
  - **Task 14**: `internal/admin/handler_fuzz_test.go` adds
    fuzz targets for `ABTestConfig`, `ConnectorTemplate`, and
    `NotificationPreference` JSON unmarshalling. `make fuzz`
    now covers `./internal/admin/...` in addition to
    `./internal/retrieval/...`.
  - **Task 15**: `docs/openapi_test.go` `requiredPaths` now
    covers every route registered in `cmd/api/main.go`. Missing
    entries (notifications, connector-template
    get/delete, experiment get/delete, chunks quality-report,
    sources sync-history, tenants cache-config / latency-budget
    / usage, sources schema / preview / embedding,
    isolation-check, chunks/{chunk_id}) were added to
    `docs/openapi.yaml` and pinned by the test.
  - **Task 16**: `docs/runbooks/runbook_test.go` walks the
    connector registry, asserts each connector has a matching
    `<name>.md`, and pins the four required sections
    (credential rotation, quota / rate-limit, outage detection,
    error codes) per file.
  - **Task 17**: `.github/workflows/ci.yml` audit. Added
    `fast-proto-check` job (runs `make proto-check`); aggregated
    into `Required CI (fast lane)`. Full lane gains
    `full-connector-smoke`, `full-bench-e2e`,
    `full-capacity-test`; nightly cron now runs `make fuzz`.
  - **Tasks 18–20**: PROGRESS.md, PHASES.md, ARCHITECTURE.md,
    and README.md refreshed with the Round-10 deltas and the
    new wiring patterns.

- 2026-05-11: **Round 9: Next 20 tasks — finish GORM cutover, pipeline hardening, regression manifest, recording rules, pool health, openapi/doc audit**.
  - **Tasks 1–5 (GORM store migrations)**: the last five in-memory
    fakes in `cmd/api/main.go` are now GORM-backed.
    - Task 1 → `internal/admin/notification_gorm.go` (+ SQLite tests in
      `notification_gorm_test.go`) replaces `NewInMemoryNotificationStore`.
    - Task 2 → `internal/admin/abtest_gorm.go` (+
      `abtest_gorm_test.go`) replaces `NewInMemoryABTestStore`.
    - Task 3 → `internal/admin/connector_template_gorm.go` (+
      `connector_template_gorm_test.go`) replaces
      `NewInMemoryConnectorTemplateStore`.
    - Task 4 → `internal/retrieval/synonym_store_gorm.go` (+
      `synonym_store_gorm_test.go`) + migration
      `migrations/032_synonyms.sql` (and matching rollback). Replaces
      `retrieval.NewInMemorySynonymStore`.
    - Task 5 → `internal/admin/chunk_quality_gorm.go` (+
      `chunk_quality_gorm_test.go`) replaces
      `NewInMemoryChunkQualityStore`.
  - **Task 6**: Post-merge cross-backend dedup in `internal/retrieval/merger.go`
    collapses duplicate chunk IDs (highest score wins) before reranking. Test
    in `internal/retrieval/merger_test.go`.
  - **Task 7**: `POST /v1/retrieve/batch` now honours `explain:true` and fans
    the per-request explain trace through. Test in
    `internal/retrieval/batch_handler_test.go`.
  - **Task 8**: Per-stage timeout env vars
    `CONTEXT_ENGINE_FETCH_TIMEOUT` / `_PARSE_TIMEOUT` / `_EMBED_TIMEOUT` /
    `_STORE_TIMEOUT` wired into `pipeline.CoordinatorConfig`; each retry
    attempt gets its own `context.WithTimeout`. Test in
    `internal/pipeline/coordinator_test.go`.
  - **Task 9**: `CONTEXT_ENGINE_CACHE_WARM_ON_MISS=true` makes the retrieval
    handler write to the cache asynchronously (`context.WithoutCancel`) so the
    response returns before Redis flushes. Test in `internal/retrieval/handler_test.go`.
  - **Task 10**: Prometheus gauge `context_engine_grpc_circuit_breaker_state{target}`
    (0=closed, 1=half-open, 2=open) emitted from
    `internal/grpcpool/pool.go`. Mapping is decoupled from the `State` iota via
    `gaugeValueForState`. Test in `pool_test.go`.
  - **Task 11**: `services/graphrag/test_graphrag.py` — 13 unit tests covering
    `RegexExtractor` (entities, edges, stopwords, acronyms, idempotence,
    empty input) and `GraphRAGServicer` initialization. `make services-test`
    picks it up.
  - **Task 12**: Admin error-catalog audit — 7 new codes added to
    `internal/errors/catalog.go` (`ERR_CACHE_WARM_FAILED`, `ERR_BUDGET_INVALID`,
    `ERR_BUDGET_LOOKUP_FAILED`, `ERR_CACHE_CONFIG_FAILED`,
    `ERR_SYNC_HISTORY_FAILED`, `ERR_PINNED_RESULTS_FAILED`,
    `ERR_PIPELINE_HEALTH_FAILED`). Tests in `catalog_test.go`.
  - **Task 13**: `tests/e2e/notification_lifecycle_test.go` (build tag `e2e`)
    drives the full notification dispatch → retry → dead-letter loop.
  - **Task 14**: `tests/e2e/pipeline_priority_test.go` (build tag `e2e`)
    asserts steady-state events drain before backfill when
    `CONTEXT_ENGINE_PRIORITY_ENABLED=true` and that near-duplicate chunks
    are dropped when `CONTEXT_ENGINE_DEDUP_ENABLED=true`.
  - **Task 15**: `tests/regression/round78_manifest.go` catalogues the
    Devin Review fixes from PRs #16 and #17 — pin_apply sparse positions,
    budget cardinality, cache_warm err, 429 retry handling, retry worker
    attempt counter, phantom-notification on rollback, etc. Meta-tests in
    `round78_manifest_test.go`.
  - **Task 16**: `deploy/recording-rules.yaml` ships
    `context_engine_retrieval_availability`, `..._p95_latency_ms`,
    `..._pipeline_throughput_per_minute`, `..._error_rate_per_minute`, and
    `..._cache_hit_rate`. `make alerts-check` now validates both alerts
    and recording rules; `internal/observability/alertcheck/main.go` accepts
    multiple files and recognises the `record` rule form. Tests in
    `deploy/alerts_test.go`.
  - **Task 17**: Connection-pool health gauges
    `context_engine_postgres_pool_open_connections`,
    `context_engine_redis_pool_active_connections`, and
    `context_engine_qdrant_pool_idle_connections` plus a 30-second sampler
    goroutine wired in `cmd/api/main.go`. Sampler logic in
    `internal/observability/pool_sampler.go`; tests in
    `pool_sampler_test.go`.
  - **Task 18**: OpenAPI spec audit — `/v1/admin/sources/{id}/rotate-credentials`,
    `/v1/admin/retrieval/pins[/{id}]`, `/v1/admin/analytics/queries[/top]`,
    `/v1/admin/health/indexes`, `/v1/admin/sources/{id}/sync/stream`,
    `/v1/webhooks/{connector}/{source_id}`, and `/v1/admin/dlq/replay` added
    to `docs/openapi.yaml`. Pinned by `docs/openapi_test.go` so future
    handlers can't ship without a spec entry.
  - **Task 19**: This changelog entry + Round-9 status note in `PHASES.md` +
    Round-9 tech-choices section in `ARCHITECTURE.md`.
  - **Task 20**: README Round-9 section + project-structure tree refresh.

- 2026-05-11: **Round 8: Next 20 tasks — pipeline wiring, GORM stores, notification delivery, e2e, runbook, docs**.
  - **Task 1**: `pipeline.Deduplicator` wired into Stage 4 store worker; gated
    by `CONTEXT_ENGINE_DEDUP_ENABLED`. Test:
    `internal/pipeline/round8_wiring_test.go`.
  - **Task 2**: `pipeline.PriorityBuffer` routed through coordinator `Submit`
    when `CONTEXT_ENGINE_PRIORITY_ENABLED=true`. Test:
    `internal/pipeline/round8_wiring_test.go`.
  - **Task 3**: `admin.EmbeddingConfigRepository` consulted by Stage 3 embed
    worker for per-source model overrides. Test: `internal/pipeline/embed_test.go`.
  - **Task 4**: `pipeline.RetryAnalytics` recording every retry/success/failure
    in `coordinator.runWithRetry`. Test: `internal/pipeline/round8_wiring_test.go`.
  - **Task 5**: Notification dispatcher fans audit events out to webhook /
    email subscribers. Wired in `cmd/api/main.go`; integration test in
    `internal/admin/notification_audit_test.go`.
  - **Task 6**: GORM-backed `QueryAnalyticsStoreGORM` swapped for the in-memory
    fake in `cmd/api/main.go`; SQLite test in `internal/admin/round8_gorm_test.go`.
  - **Task 7**: GORM-backed `PinnedResultStoreGORM` in
    `internal/admin/pinned_results_gorm.go`; wired via
    `Handler.SetPinLookup`; tests in `round8_gorm_test.go` and
    `internal/retrieval/round8_handler_test.go`.
  - **Task 8**: GORM-backed `SyncHistoryGORM` wired in `cmd/api/main.go`;
    test in `round8_gorm_test.go`.
  - **Task 9**: GORM-backed `LatencyBudgetGORM` + `Handler.SetLatencyBudgetLookup`
    so per-tenant `max_latency_ms` bounds the request context. Tests in
    `round8_gorm_test.go` and `round8_handler_test.go`.
  - **Task 10**: `CacheTTLGORM` + `Handler.SetCacheTTLLookup` so per-tenant
    cache TTL is applied on every `cache.Set`. Tests in `round8_gorm_test.go`
    and `round8_handler_test.go`.
  - **Task 11**: GORM-backed `CredentialHealthGORM` + periodic
    `CredentialHealthWorker` in `cmd/ingest/main.go` (interval via
    `CONTEXT_ENGINE_CREDENTIAL_HEALTH_INTERVAL`). Test in `round8_gorm_test.go`.
  - **Task 12**: Migrations 024–031 + rollbacks verified present.
  - **Task 13**: `docs/openapi.yaml` extended with Round 5/6/7 endpoints.
  - **Task 14**: `tests/e2e/round8_test.go` (build tag `e2e`) round-trips all
    Round-8 admin handlers.
  - **Task 15**: A/B router stamps `experiment_name` / `experiment_arm` on the
    `query_analytics` event; test in `round8_handler_test.go`.
  - **Task 16**: `pin_apply.ApplyPins` invoked in retrieval handler after
    policy filtering and before caching; test in `round8_handler_test.go`.
  - **Task 17**: Notification retry worker + persisted `next_retry_at` with
    linear backoff and dead-letter after `DefaultMaxRetryAttempts`. Tests in
    `internal/admin/notification_retry_worker_test.go`.
  - **Task 18**: `.github/workflows/ci.yml` extended with fast-lane jobs for
    `make alerts-check` and migration/rollback parity tests.
  - **Task 19**: `docs/runbooks/operational.md` covering Round 6/7/8
    operational procedures.
  - **Task 20**: Documentation audit — this changelog, README Round-8 section,
    ARCHITECTURE Round-8 tech-choices section, PHASES.md status note.

- 2026-05-11: **Round 7: Next 20 tasks — analytics, wiring, hardening, rollbacks**.
  - **Task 1**: Rollback scripts for migrations 015–031 — every numeric prefix
    under `migrations/` now has a matching `migrations/rollback/NNN_*.down.sql`;
    `rollback_test.go` enforces the contract.
  - **Task 2**: Full Round-6 wiring into `cmd/api/main.go` — `APIVersionMiddleware`,
    `ABTestHandler`+`ABTestResultsHandler`, `ConnectorTemplateHandler`,
    `NotificationHandler`+`NotificationDeliveryLogHandler`,
    `EmbeddingConfigHandler`, `RetryStatsHandler`, `SynonymsHandler`,
    `QueryAnalyticsHandler`, `CredentialHealthHandler`, `CacheWarmHandler`,
    `BulkSourceHandler`, plus every Round-7 admin handler (latency budget,
    chunk quality, audit export, cache config, sync history, pinned results,
    pipeline health). New `Handler.SetABTestRouter` /
    `Handler.SetQueryAnalyticsRecorder` setters keep the admin↔retrieval
    import graph one-way.
  - **Task 3**: Round-6 ingest wiring in `cmd/ingest/main.go` — semantic dedup
    behind `CONTEXT_ENGINE_DEDUP_ENABLED`, priority buffer behind
    `CONTEXT_ENGINE_PRIORITY_ENABLED`, embedding config resolver, retry
    analytics aggregator constructed alongside the coordinator.
  - **Task 4**: Retrieval query analytics —
    `internal/admin/query_analytics.go`, `migrations/024_query_analytics.sql`,
    `GET /v1/admin/analytics/queries` with `top` mode, recorded after every
    successful retrieve via the new handler hook.
  - **Task 5**: Notification dispatcher retry + dead-letter — exponential
    backoff (1s/5s/15s) inside `WebhookDelivery.Send`,
    `notification_delivery_log` table (`migrations/025_*.sql`),
    `GET /v1/admin/notifications/delivery-log`.
  - **Task 6**: A/B test results endpoint — `internal/admin/abtest_results.go`
    aggregates `query_analytics` rows per arm,
    `GET /v1/admin/retrieval/experiments/:name/results` returns
    P50/P95 latency, hit count, cache hit rate per arm.
  - **Task 7**: Credential health worker —
    `internal/admin/credential_health.go`, `migrations/026_credential_valid.sql`,
    `GET /v1/admin/sources/:id/credential-health`, emits
    `source.credential_invalid` audit events.
  - **Task 8**: GraphRAG e2e — `tests/e2e/graphrag_test.go` (build tag `e2e`)
    exercises Stage 3b extraction → FalkorDB writes → retrieval-time graph
    lookup → deletion prune.
  - **Task 9**: Retrieval cache warming —
    `internal/retrieval/cache_warmer.go` +
    `internal/admin/cache_warm_handler.go`,
    `POST /v1/admin/retrieval/warm-cache` with optional auto-top-N from
    query analytics.
  - **Task 10**: Bulk source operations —
    `internal/admin/bulk_source_handler.go`,
    `POST /v1/admin/sources/bulk` with concurrent fan-out and per-source
    isolation; emits one audit row per source.
  - **Task 11**: Per-tenant retrieval latency budget —
    `internal/admin/latency_budget.go`, `migrations/027_latency_budgets.sql`,
    `GET/PUT /v1/admin/tenants/:id/latency-budget`.
  - **Task 12**: Chunk quality scoring —
    `internal/pipeline/chunk_scorer.go`, `migrations/028_chunk_quality.sql`,
    `GET /v1/admin/chunks/quality-report` aggregated per source.
  - **Task 13**: Source sync conflict resolution —
    `internal/pipeline/conflict_resolver.go` with last-writer-wins and
    monotonic `content_version`; emits `chunk.conflict_resolved` audit events.
  - **Task 14**: Audit trail export — `internal/admin/audit_export.go`,
    `GET /v1/admin/audit/export?format=csv|jsonl&...` streamed with chunked
    transfer encoding.
  - **Task 15**: Per-tenant cache TTL —
    `internal/admin/cache_config.go`, `migrations/029_cache_config.sql`,
    `GET/PUT /v1/admin/tenants/:id/cache-config`.
  - **Task 16**: Connector sync history —
    `internal/admin/sync_history.go`, `migrations/030_sync_history.sql`,
    `GET /v1/admin/sources/:id/sync-history?limit=N`.
  - **Task 17**: Retrieval result pinning —
    `internal/admin/pinned_results.go` (CRUD) +
    `internal/retrieval/pin_apply.go` (merge), `migrations/031_pinned_results.sql`,
    `POST/GET/DELETE /v1/admin/retrieval/pins`.
  - **Task 18**: Pipeline stage health dashboard —
    `internal/admin/pipeline_health.go`, `GET /v1/admin/pipeline/health`
    reads Prometheus histograms and counters for throughput, P50/P95
    latency, retry, queue depth, DLQ totals.
  - **Task 19**: Comprehensive Round-7 e2e test — `tests/e2e/round7_test.go`
    covers query analytics, notification delivery, A/B results, credential
    health, bulk source ops, cache warming, chunk quality, audit export,
    sync history, latency budget, cache TTL, and pinned results.
  - **Task 20**: Documentation updated — Round-7 sections added to
    PROGRESS.md, PHASES.md, ARCHITECTURE.md, README.md; directory trees
    refreshed; checkbox audit passed.

- 2026-05-10: **Round 6: Next 20 tasks — retrieval diversity, semantic dedup,
  query expansion, chunk ACL, streaming, and more**.
  - **Task 1**: Retrieval result diversity (MMR) — `internal/retrieval/diversifier.go`,
    wired after the reranker; configurable lambda (default 0.0 = passthrough).
  - **Task 2**: Semantic deduplication in Stage 4 — `internal/pipeline/dedup.go`,
    cosine similarity > threshold; toggled by `CONTEXT_ENGINE_DEDUP_ENABLED`.
  - **Task 3**: Per-source embedding model config — `internal/admin/embedding_config.go`,
    `migrations/019_source_embedding_config.sql`, `GET/PUT /v1/admin/sources/:id/embedding`.
  - **Task 4**: Query expansion via synonyms — `internal/retrieval/query_expander.go`,
    `internal/admin/synonyms_handler.go`, `POST /v1/admin/synonyms`.
  - **Task 5**: Priority queues in pipeline coordinator —
    `internal/pipeline/priority.go`; admin-overridable classifier.
  - **Task 6**: Chunk-level ACL — `internal/policy/chunk_acl.go`,
    `migrations/020_chunk_acl.sql`, post-merge filter in
    `internal/retrieval/policy_snapshot.go`.
  - **Task 7**: Connector adaptive rate limiting — `internal/connector/adaptive_rate.go`;
    halves on 429, gradually restores up to BaseRate.
  - **Task 8**: Source schema discovery endpoint —
    `internal/admin/schema_handler.go`, `GET /v1/admin/sources/:id/schema`.
  - **Task 9**: Pipeline backpressure metrics —
    `context_engine_pipeline_channel_depth{stage}` gauge in
    `internal/observability/metrics.go`; alert rule in
    `deploy/alerts/pipeline_backpressure.yaml`.
  - **Task 10**: Retrieval A/B testing framework — `internal/admin/abtest.go`,
    `migrations/021_ab_tests.sql`,
    `POST/GET/DELETE /v1/admin/retrieval/experiments`.
  - **Task 11**: Admin notification preferences — `internal/admin/notification.go`,
    `migrations/022_notification_preferences.sql`,
    `GET/POST/DELETE /v1/admin/notifications`.
  - **Task 12**: Shard pre-generation scheduler — `internal/shard/scheduler.go`;
    runs on a configurable tick when `CONTEXT_ENGINE_SHARD_PREGENERATE=true`.
  - **Task 13**: API versioning middleware — `internal/observability/api_version.go`;
    resolves `Accept-Version` / `/vN/` prefix, emits `X-API-Version`.
  - **Task 14**: Tenant data portability (full export) — `internal/admin/tenant_export.go`,
    `POST /v1/admin/tenants/:tenant_id/export`,
    `GET /v1/admin/tenants/:tenant_id/export/:job_id`.
  - **Task 15**: Pipeline dry-run mode — `dry_run` flag on
    `POST /v1/admin/reindex`; orchestrator enumerates without emitting.
  - **Task 16**: Connector template system — `internal/admin/connector_template.go`,
    `migrations/023_connector_templates.sql`,
    `GET/POST /v1/admin/connector-templates`.
  - **Task 17**: Cross-tenant data isolation audit —
    `internal/admin/isolation_audit.go`,
    `POST /v1/admin/isolation-check`.
  - **Task 18**: Retrieval response streaming (SSE) —
    `internal/retrieval/stream_handler.go`,
    `POST /v1/retrieve/stream`.
  - **Task 19**: Pipeline stage retry analytics —
    `internal/pipeline/retry_analytics.go`,
    `internal/admin/retry_stats_handler.go`,
    `GET /v1/admin/pipeline/retry-stats`,
    `context_engine_pipeline_retries_total{stage,outcome}` counter.
  - **Task 20**: Round-6 comprehensive e2e test —
    `tests/e2e/round6_test.go` (build tag `e2e`).

- 2026-05-10: **PR #14 — Round 5: CI fix + next-20 tasks**. CI Task 0
  fixed sidecar Dockerfiles (missing `_metrics.py` COPY). Tasks 1–20:
  - **Task 1**: Eval golden dataset + CI retrieval quality gate
    (`tests/eval/golden_corpus.json`, `tests/eval/eval_ci_test.go`,
    `make eval`).
  - **Task 2**: Connector OAuth token auto-refresh worker
    (`internal/admin/token_refresh.go`).
  - **Task 3**: Cursor-based pagination for admin list endpoints
    (`internal/admin/pagination.go`).
  - **Task 4**: RBAC role middleware (`internal/admin/rbac.go`).
  - **Task 5**: Generic webhook delivery router
    (`internal/admin/webhook_router.go`).
  - **Task 6**: Grafana dashboard JSON model
    (`deploy/grafana/context-engine-dashboard.json`,
    `docs/runbooks/alerting.md`).
  - **Task 7**: GDPR data export endpoint
    (`internal/admin/data_export.go`,
    `migrations/015_export_jobs.sql`).
  - **Task 8**: Namespace-level privacy mode override
    (`internal/policy/namespace_policy_test.go`,
    `migrations/016_namespace_policies.sql`).
  - **Task 9**: Chunk provenance trace API
    (`internal/admin/provenance_handler.go`).
  - **Task 10**: Connector dry-run preview
    (`internal/admin/preview_handler.go`).
  - **Task 11**: DLQ batch replay with filters
    (`internal/admin/dlq_batch_replay.go`).
  - **Task 12**: Retrieval explain/debug mode
    (`internal/retrieval/explain.go`).
  - **Task 13**: Backfill completion event + SSE notification
    (`internal/admin/backfill_completion.go`).
  - **Task 14**: Credential expiry monitoring worker
    (`internal/admin/credential_monitor.go`).
  - **Task 15**: Policy version history + rollback
    (`internal/policy/version_history.go`,
    `internal/admin/policy_history_handler.go`,
    `migrations/017_policy_versions.sql`).
  - **Task 16**: Retrieval feedback collection
    (`internal/retrieval/feedback_handler.go`,
    `internal/eval/feedback.go`,
    `migrations/018_feedback_events.sql`).
  - **Task 17**: Idempotency key middleware
    (`internal/admin/idempotency.go`).
  - **Task 18**: Stale-index watchdog + auto-reindex
    (`internal/admin/index_watchdog.go`).
  - **Task 19**: Cross-tenant super-admin analytics
    (`internal/admin/analytics_handler.go`).
  - **Task 20**: PR #13 features e2e integration test
    (`tests/e2e/pr13_features_test.go`).

- 2026-05-10: **PR #13 — Round 4: next-20 tasks**. Eval runner,
  error catalogue, scheduler, metering, credential rotation,
  GraphRAG integration, fault injection, Python services,
  regression tests, operational tooling (alerts, index health,
  reindex orchestrator, SSE sync progress, API rate limiter).

- 2026-05-10: Round-4 next-20 batch (eval harness, GraphRAG
  Stage 3b, production hardening, testing depth, operational
  tooling). Each task ships unit tests; full suite passes
  under `-race`. CI Task 0 stabilised the failing full-lane
  e2e tests (Postgres CHAR(N) blank-padding fix, retrieval ACL
  path resolution, healthcheck blocks for docling/embedding/
  memory). Remaining 20 tasks:
  - **Task 1: Retrieval evaluation harness**
    (`internal/eval/`, `migrations/012_eval_suites.sql`,
    `GET /v1/admin/eval/run`). EvalSuite holds (query,
    expected_chunk_ids, expected_min_score) tuples;
    `metrics.go` computes Precision@K / Recall@K / MRR / NDCG
    against the retrieval handler; the runner returns an
    EvalReport; the admin handler exposes the run trigger.
  - **Task 2: GraphRAG Stage 3b entity extraction**
    (`proto/graphrag/v1/graphrag.proto`,
    `services/graphrag/`, `internal/pipeline/graphrag.go`).
    Optional pipeline hook after Embed and before Store,
    gated by `CONTEXT_ENGINE_GRAPHRAG_ENABLED`. Writes
    nodes/edges to FalkorDB through
    `internal/storage/falkordb.go`.
  - **Task 3: Webhook HMAC signature verification**
    (`internal/connector/webhook_verify.go`). GitHub /
    GitLab / Jira / Teams / Slack signature schemes all
    funnel through one `VerifyHMAC(secret, payload, signature, algo)`.
    Per-connector tests cover valid/invalid signatures.
  - **Task 4: Retention policy enforcement worker**
    (`internal/admin/retention_worker.go`). Periodic
    sweeper queries chunks past their tenant TTL, fans out
    to ForgetSweeper implementations, emits the
    `chunk.expired` audit action.
  - **Task 5: Source sync cron scheduler**
    (`internal/admin/scheduler.go`,
    `internal/admin/cron.go`,
    `migrations/013_sync_schedules.sql`). In-house cron
    parser drives the per-(tenant, source) schedule;
    scheduler goroutine in `cmd/ingest` emits Kafka ingest
    events when `next_run_at <= now()`.
  - **Task 6: Startup config validation**
    (`internal/config/validate.go`). Validates required
    `CONTEXT_ENGINE_*` env vars on boot; checks numeric
    ranges and parseable URLs; logs warnings for unset
    optional vars with their defaults. Wired into both
    `cmd/api` and `cmd/ingest` as the first startup step.
  - **Task 7: Structured error catalog**
    (`internal/errors/catalog.go`,
    `internal/errors/middleware.go`). Typed error codes
    (`ERR_TENANT_NOT_FOUND`, `ERR_RATE_LIMITED`, ...) with
    HTTP status + retry hint; Gin error middleware maps
    Go errors to structured JSON.
  - **Task 8: Per-tenant API rate limiting middleware**
    (`internal/admin/api_ratelimit.go`). Redis sliding
    window distinct from the per-source ratelimit; per-
    endpoint-class limits (retrieval 100/min, admin
    30/min); `429 Too Many Requests` with `Retry-After`.
  - **Task 9: Request ID propagation through Kafka**
    (`pipeline.IngestEvent.RequestID`). Producer pulls
    request_id from gin context; consumer logs it on
    every stage; DLQ records the original request id.
  - **Task 10: Prometheus alerting rules**
    (`deploy/alerts.yaml`,
    `internal/observability/alertcheck/`). PrometheusRule
    manifest with IngestionLagHigh / DLQRateHigh /
    RetrievalP95High / SourceUnhealthy / pipeline-stage
    duration. `make alerts-check` validates the YAML.
  - **Task 11: Fuzz tests for retrieval query parsing**
    (`internal/retrieval/handler_fuzz_test.go`). Three Go
    native fuzz targets cover JSON unmarshal robustness,
    ACL evaluation with random rules, privacy-mode
    coercion. `make fuzz` target.
  - **Task 12: Full pipeline integration test**
    (`tests/integration/pipeline_full_test.go`). In-process
    Stage 1→4 with fakes for fetch/parse/embed/store;
    happy-path + DLQ-routing + request-id-propagation
    cases.
  - **Task 13: Proto contract tests**
    (`tests/integration/proto_compat_test.go`). Static
    shape validation of Go and Python proto stubs;
    field-number preservation; backward-compat (no
    duplicate field numbers).
  - **Task 14: Chaos / fault injection hooks**
    (`internal/storage/fault.go`). FaultInjector wraps
    storage clients; configurable error rate, latency,
    timeout. Off by default; opt-in via
    `CONTEXT_ENGINE_FAULT_INJECTION=true`.
  - **Task 15: Regression test suite for PR #12 bug fixes**
    (`tests/regression/manifest.go`). Catalogues each PR
    #12 finding alongside the regression test that pins
    its fix. Three meta-tests defend the manifest itself.
  - **Task 16: Admin API for connector credential rotation**
    (`internal/admin/credential_rotation.go`,
    `POST /v1/admin/sources/:id/rotate-credentials`,
    `audit.ActionSourceCredentialsRotated`). Validates
    new credentials via the connector's Validate before
    swapping; previous credential held for
    CredentialGracePeriod (1h) so in-flight requests drain.
  - **Task 17: Tenant usage metering endpoints**
    (`internal/admin/metering.go`,
    `migrations/014_tenant_usage.sql`,
    `GET /v1/admin/tenants/:id/usage?from=&to=`). Daily
    rollup of API calls / ingestion / chunk count per
    tenant. In-process Counter buffers increments and
    flushes via `FlushOnInterval`. Cross-tenant reads 403.
  - **Task 18: Source sync progress SSE endpoint**
    (`internal/admin/sync_progress_stream.go`,
    `GET /v1/admin/sources/:id/sync/stream`). Polls the
    SyncProgressStore and streams discovered / processed
    / failed / completed / heartbeat events. Settles +
    grace before sending `completed`.
  - **Task 19: Index health check endpoint**
    (`internal/admin/index_health.go`,
    `GET /v1/admin/health/indexes`). Parallel
    BackendChecker per backend (postgres / qdrant /
    redis); 200 when all green, 503 when any
    unhealthy. Per-backend latency + checked_at
    surfaced in the response.
  - **Task 20: Migration rollback scripts**
    (`migrations/rollback/`). One `NNN_*.down.sql` per
    forward migration 001-014; `make migrate-rollback`
    walks them in reverse against
    `CONTEXT_ENGINE_DATABASE_URL`. Tests confirm presence
    + non-empty per rollback.

- 2026-05-10: Phase 8 production-hardening (20-task next-batch
  landed in PR #12). Each task ships unit tests; full suite
  passes under `-race`.
  - **Task 1–4: Test backfill audit**. Confirmed `internal/b2c/`,
    `internal/models/`, `internal/observability/`, and
    `internal/grpcpool/` already carry comprehensive coverage;
    added the missing capabilities-empty-array regression in
    `internal/b2c/handler_test.go::TestHandler_Capabilities_EmptyBackendsRendersJSONArray`
    and pinned the `Capabilities.EnabledBackends` JSON shape to
    `[]` even when no backends are configured.
  - **Task 5: DLQ admin surface**.
    `internal/admin/dlq_handler.go` mounts
    `GET /v1/admin/dlq`, `GET /v1/admin/dlq/:id`, and
    `POST /v1/admin/dlq/:id/replay`. `migrations/009_dlq_messages.sql`
    persists every dead-letter envelope; the replay handler
    enforces `max_retries` to prevent infinite loops.
  - **Task 6: Retention policy enforcement**.
    `internal/policy/retention.go` (RetentionPolicy +
    layered tenant/source/namespace scope) and
    `internal/pipeline/retention_worker.go` (periodic sweep
    over Qdrant / FalkorDB / Tantivy / Postgres) ship the
    Lifecycle Management point 5 of `docs/PROPOSAL.md` §5.
    `migrations/010_retention_policy.sql` backs the rules.
  - **Task 7: Reindex pipeline**.
    `internal/pipeline/reindex.go` enumerates documents by
    `(tenant_id, source_id, [namespace_id])` and re-emits
    Stage 2–4 events without re-fetching.
    `internal/admin/reindex_handler.go` mounts
    `POST /v1/admin/reindex`. The orchestrator is wired into
    `cmd/api/main.go` behind `CONTEXT_ENGINE_KAFKA_BROKERS`.
  - **Task 8: Public-API rate limit**.
    `internal/admin/api_ratelimit.go` adds a Gin middleware on
    the `/v1/` group keyed on tenant_id using the existing
    Redis token bucket from `internal/admin/ratelimit.go`.
    Configurable via `CONTEXT_ENGINE_API_RATE_LIMIT`. Returns
    HTTP 429 with `Retry-After`.
  - **Task 9: Webhook signature verification**.
    `internal/connector/webhook_verify.go` provides the shared
    HMAC-SHA256 / SHA-1 / token verifiers. Wired into the four
    `WebhookReceiver` connectors (jira, github, gitlab, teams).
    Each connector ships its own
    `webhook_verify_test.go` covering valid / invalid /
    missing-header cases.
  - **Task 10: Graceful shutdown**.
    `internal/lifecycle/shutdown.go` provides an ordered
    `Step` runner with a deadline budget. `cmd/api/main.go`
    drains in-flight HTTP, then closes Postgres / Redis;
    `cmd/ingest/main.go` stops the consumer, drains the
    pipeline coordinator, then closes the HTTP probe and
    Postgres / Redis. Configurable via
    `CONTEXT_ENGINE_SHUTDOWN_TIMEOUT_SECONDS`.
  - **Task 11: Configuration validation**.
    `internal/config/validate.go` aggregates required-env-var
    + URL-format checks into a single structured `ConfigError`
    (`Looker` interface lets tests pass a `MapLooker` instead
    of `os.Getenv`). `ValidateAPI` / `ValidateIngest` run
    before any `gorm.Open` / `redis.NewClient` call.
  - **Task 12: Bulk retrieval**.
    `internal/retrieval/batch_handler.go` mounts
    `POST /v1/retrieve/batch`, fans requests out concurrently
    with a configurable `max_parallel` cap (default 8, hard
    limit of 32 sub-requests), and isolates per-request policy
    so one failed query does not fail the batch.
  - **Task 13: Audit log search/filter**.
    `internal/audit/repository.go::ListFilter` adds
    `ResourceID` and `PayloadSearch`; `internal/audit/handler.go`
    binds the new `source_id=` / `resource_id=` / `q=` query
    params and additionally mounts the search surface on
    `GET /v1/admin/audit` (the legacy `/v1/audit-logs` path is
    preserved for back-compat).
  - **Task 14: Sync progress tracking**.
    `internal/admin/sync_progress.go` adds a GORM-backed
    `SyncProgressStore` keyed on
    `(tenant_id, source_id, namespace_id)` with UPSERT
    increment helpers; `internal/admin/sync_progress_handler.go`
    mounts `GET /v1/admin/sources/:id/progress` returning
    discovered / processed / failed / percent_done per
    namespace.
  - **Task 15: Tenant-deletion e2e**.
    `tests/e2e/tenant_deletion_test.go` extends the Phase 5
    forget smoke into a full deletion flow: seed shards + audit
    rows, call `DELETE /v1/tenants/:tenant_id/keys`, assert
    trigger fired exactly once, assert subsequent shard list is
    empty, assert audit log is *retained* (forensics + governance).
  - **Task 16: Degradation / fault injection**.
    `tests/e2e/degradation_test.go` covers vector-down,
    cache-down, slow-backend, and all-backends-down paths.
    Asserts `policy.degraded` carries the failed backend and
    the response is never 5xx.
  - **Task 17: OpenAPI spec**.
    `docs/openapi.yaml` documents every public route — health
    probes, B2C surfaces, retrieval (single + batch), shards
    (list, delta, coverage, forget), models catalog, audit
    (legacy + admin), admin sources / health / progress /
    dashboard / DLQ (list + replay) / reindex / policy
    (drafts + simulate + conflicts) / tenant deletion.
  - **Task 18: Migration runner**.
    `internal/migrate/runner.go` scans
    `migrations/*.sql`, parses `NNN_name.sql` order, and
    applies pending migrations inside per-file transactions
    while recording rows in `schema_migrations`.
    `DryRun` returns the pending list without applying.
    Wired into `cmd/api/main.go` and `cmd/ingest/main.go`
    behind `AUTO_MIGRATE=true` (or
    `CONTEXT_ENGINE_AUTO_MIGRATE=true`); migrations dir is
    `CONTEXT_ENGINE_MIGRATIONS_DIR` (default `migrations`).
  - **Task 19: Connector dashboard**.
    `internal/admin/dashboard_handler.go` mounts
    `GET /v1/admin/dashboard` and aggregates
    `source_health` rows by status (healthy / degraded /
    failing / unknown). When a `MetricsSnapshot` is provided
    the response also carries pipeline throughput, retrieval
    P95, and per-backend availability flags. The widget shape
    is intentionally `additionalProperties: true` in OpenAPI
    so the admin portal can iterate without breaking server
    changes.
  - **Task 20: Request ID middleware**.
    `internal/observability/request_id.go` reads the inbound
    `X-Request-ID` (rejecting control chars / >128 chars and
    minting a ULID otherwise), binds it to the gin context,
    request context, response header, and the per-request
    `slog` logger (`request_id` field). Mounted ahead of all
    other middleware in `cmd/api/main.go` so every downstream
    log line and tracing span correlates.
  - **Documentation**: this changelog entry, plus the
    `docs/openapi.yaml` spec generated by Task 17. Phase 8
    line-items in this section remain at ~100% (all 20 tasks
    landed); the qualitative "production-hardening" status
    for Phase 8 moves from "feature complete + observed" to
    "feature complete + observed + admin-surface complete".

- 2026-05-10: Phase 5/6 wiring closeout + observability + benchmarks +
  e2e closeout (20-task batch landed in this PR):
  - **Phase 5 wiring closeout**:
    - `migrations/007_channel_deny_local.sql` adds the
      `deny_local_retrieval BOOLEAN NOT NULL DEFAULT FALSE` column
      to `channel_policies`. `internal/policy/live_resolver.go`
      now reads the column on every `Resolve()` call and
      populates `policy.PolicySnapshot.DenyLocalRetrieval` —
      completing the Phase 6 `channel_disallowed` reason wiring
      that was previously snapshot-only.
    - `internal/shard/coverage_repo.go` ships the GORM-backed
      `CoverageRepoGORM` so the coverage endpoint can return
      `is_authoritative=true`. `cmd/api/main.go` wires it via
      `shard.HandlerConfig{CoverageRepo: …}`.
  - **Phase 8 operational wiring closeout**:
    - `cmd/ingest/main.go` reads
      `CONTEXT_ENGINE_FETCH_WORKERS` /
      `CONTEXT_ENGINE_PARSE_WORKERS` /
      `CONTEXT_ENGINE_EMBED_WORKERS` /
      `CONTEXT_ENGINE_STORE_WORKERS` and threads them through
      `pipeline.StageConfig` so per-stage worker pools can be
      tuned without a redeploy.
    - `cmd/api/readyz.go` and `cmd/ingest/health.go` ship the
      `/healthz` + `/readyz` probes (Postgres / Redis / Qdrant /
      Kafka). Wired into the same listener as `/metrics`.
  - **Phase 5/6 e2e closeout**:
    - `tests/e2e/phase5_shard_test.go`,
      `tests/e2e/phase5_models_test.go`,
      `tests/e2e/phase5_forget_test.go`,
      `tests/e2e/phase6_b2c_test.go`, and
      `tests/e2e/phase6_device_first_test.go` cover
      `/v1/shards/:tenant_id` + delta + coverage,
      `/v1/models/catalog`, the cryptographic-forget
      DELETE endpoint, the B2C `/v1/health` /
      `/v1/capabilities` / `/v1/sync/schedule` surfaces, and
      the device-first `prefer_local` hint on
      `POST /v1/retrieve`. All five run under the existing
      `e2e` build tag and `make test-e2e` target.
  - **Phase 1 / Phase 3 P95 budgets**:
    - `tests/benchmark/p95_e2e_test.go` enforces the Phase 1
      end-to-end round-trip budget (< 1 s P95).
    - `tests/benchmark/p95_retrieval_test.go` enforces the
      stricter Phase 3 retrieval-only budget (< 500 ms P95).
    - `make bench-e2e` runs both suites under the `e2e` build
      tag so CI can fail on regression.
  - **Phase 8 pipeline completeness**:
    - `internal/pipeline/dlq_observer.go` + Prometheus counter
      `context_engine_dlq_messages_total{original_topic}`
      (per-tenant breakdowns live in the `tenant_id` log
      field — see "Logger middleware key fix" below).
      Logs structured fields per dead-letter envelope
      (`tenant_id`, `document_id`, `source_id`, `error`,
      `attempt_count`, `original_topic`, `timestamp`).
      Optionally wired in `cmd/ingest/main.go` behind
      `CONTEXT_ENGINE_DLQ_OBSERVE=1`.
    - `internal/admin/tenant_delete.go` implements the 5-step
      tenant-deletion workflow from `docs/ARCHITECTURE.md` §5
      (mark pending → drain → sweep derived data → destroy
      DEKs → mark deleted). `migrations/008_tenant_status.sql`
      adds the `tenants.tenant_status` column with
      `idx_tenants_status`. `DELETE /v1/admin/tenants/:tenant_id`
      mounts the workflow under the admin handler.
  - **Phase 6 / Phase 8 observability**:
    - `internal/observability/logger.go` wraps `log/slog` with a
      JSON handler that always emits `tenant_id` / `trace_id` /
      `component` / `level` / `msg` / `timestamp`.
      `LoggerFromContext(ctx)` extracts the active logger
      pre-bound with the request scope.
    - `GinLoggerMiddleware(component)` parses W3C `traceparent`
      (with `X-Trace-ID` fallback) and reads the
      `audit.TenantContextKey` set by the auth layer (with an
      `X-Tenant-Id` header fallback) to inject the per-request
      logger. Mounted on the authed `cmd/api` route group; the
      `cmd/ingest` binary uses `net/http` and gets the same
      structured `slog` JSON output via `slog.SetDefault` instead
      of a Gin middleware.
  - **Quality / robustness**:
    - `internal/policy/device_first_test.go` adds the
      `TestDecide_FullMatrix` 5×3 truth table over privacy modes
      × device tiers, plus precedence and nil-snapshot guards.
    - `internal/shard/eviction_test.go` adds
      `TestShouldEvict_FullMatrix` covering the 24-row tier ×
      thermal × memory × shard truth table and
      `TestDefaultEvictionPolicies_ExactValues` pinning the
      three Phase 8 default thresholds.
  - **Documentation audit**: `docs/PROGRESS.md`,
    `docs/PHASES.md`, `docs/ARCHITECTURE.md`, and `README.md`
    re-cross-referenced — every checkbox in PROGRESS now matches
    code on disk; PHASES exit criteria reflect the P95 + readyz
    + e2e landings; ARCHITECTURE §9 directory tree includes the
    new files; README's status banner + Make targets table
    surface `bench-e2e` and the new percentages.

- 2026-05-10: Phase 5 contracts → ~70% + Phase 6 server-side
  bootstrap → ~60% + Phase 8 on-device contracts → ~100%:
  - **Phase 5 / on-device contracts**: Go-side
    `ShardClientContract` interface
    (`internal/shard/contract.go`) defines the four-method
    contract every on-device runtime (iOS XCFramework, Android
    AAR, Electron N-API addon) must implement.
    `docs/contracts/uniffi-ios.md`,
    `docs/contracts/uniffi-android.md`, and
    `docs/contracts/napi-desktop.md` document the
    platform-specific packaging, IPC model, and cryptographic
    forget steps the runtimes must perform.
  - **Phase 5 / coverage endpoint**:
    `GET /v1/shards/:tenant_id/coverage` (handler in
    `internal/shard/handler.go`) returns shard / corpus chunk
    counts plus `is_authoritative` so the
    `docs/contracts/local-first-retrieval.md` decision tree can
    fire on the client. The `CoverageRepo` port is optional;
    when unimplemented the response sets `is_authoritative=false`
    and clients treat the ratio as advisory.
  - **Phase 5 / Bonsai integration**: `internal/models/`
    ships `ModelCatalog`, `ModelEntry`, `Provider` /
    `StaticProvider`, and a baseline catalog of three
    Bonsai-1.7B builds (q4_0 / q8_0 / fp16) with
    `EligibleForTier` returning the smallest matching model.
    `GET /v1/models/catalog` is mounted in `cmd/api/main.go`.
    `docs/contracts/bonsai-integration.md` documents the wire
    format, tier eligibility, and download flow.
  - **Phase 6 / B2C client SDK**: `internal/b2c/handler.go`
    serves `GET /v1/health`, `GET /v1/capabilities`, and
    `GET /v1/sync/schedule`. The capabilities response reports
    enabled retrieval backends, supported privacy modes, and
    the `device_first` / `local_shard_sync` feature flags so a
    B2C UI built against an older server can downgrade
    gracefully. `docs/contracts/b2c-retrieval-sdk.md` is the
    SDK contract.
  - **Phase 6 / device-first policy**:
    `internal/policy/device_first.go::Decide` returns a
    structured `DeviceFirstDecision` (prefer_local +
    local_shard_version + reason). The retrieval handler
    consults it on every successful `POST /v1/retrieve` (cache
    hit + fresh path) and surfaces the result on the response
    envelope (`RetrieveResponse.prefer_local`,
    `.local_shard_version`, `.prefer_local_reason`).
    `shard.VersionLookup` is the adapter from the shard
    repository to the retrieval handler's narrow
    `ShardVersionLookup` port; lookup failures fail closed to
    `prefer_local=false`.
  - **Phase 6 / privacy strip render contract**:
    `docs/contracts/privacy-strip-render.md` is the per-platform
    render guidance the admin portal, desktop, iOS, and Android
    UIs implement against the Phase 4 server-side enrichment.
  - **Phase 6 / background sync contract**:
    `docs/contracts/background-sync.md` documents the
    platform-native schedulers (iOS `BGAppRefreshTask`, Android
    `WorkManager`, Electron `powerMonitor`) and the
    `GET /v1/sync/schedule` endpoint they consume; the schedule
    enforces minimum-interval floors so a rogue release can't
    DoS the API.
  - **Phase 8 / Bonsai benchmark contract**:
    `tests/benchmark/bonsai_contract_test.go::BonsaiContract`
    defines the per-tier performance envelope (Low: ≥4 tok/s,
    Mid: ≥12 tok/s, High: ≥25 tok/s) the on-device runtimes
    must clear; `SatisfiesContract()` is the helper the
    `kennguy3n/knowledge` and `kennguy3n/llama.cpp` repos call
    against measured numbers.
  - **Phase 8 / shard eviction**: `internal/shard/eviction.go`
    ships `EvictionPolicy` / `EvictionInputs` /
    `EvictionDecision` and `ShouldEvict()` with deterministic
    `unknown_tier` / `shard_too_large` / `memory_pressure`
    labels. `DefaultEvictionPolicies()` is exposed in the
    catalog response so the policy can be tuned server-side
    without an on-device release.
- 2026-05-10: Phase 7 catalog → ~100% + Phase 8 optimisation →
  ~85% + Phase 1/3 retrieval P95 budget enforced + Phase 8
  liveness / readiness probes:
  - **Phase 7 finalisation**: 12 per-connector runbooks under
    `docs/runbooks/` (one per connector + a README index)
    covering credential rotation, quota / rate-limit incidents,
    outage detection, and error codes specific to each
    connector's auth model and delta cursor. End-to-end smoke
    suite (`tests/e2e/connector_smoke_test.go`, build tag
    `e2e`) exercises Validate → Connect → ListNamespaces →
    ListDocuments → FetchDocument for every connector; Jira,
    GitHub, GitLab, and Teams additionally run HandleWebhook;
    every `DeltaSyncer` connector runs DeltaSync. The suite
    blank-imports the catalog and asserts the registry has
    exactly 12 entries. New `make test-connector-smoke` target.
  - **Phase 8 metrics + HPA**: Prometheus client wired into the
    Go binaries (`internal/observability/metrics.go`,
    `internal/observability/middleware.go`) and the Python
    sidecars (`services/_metrics.py`,
    `services/docling/docling_server.py`,
    `services/embedding/embedding_server.py`). Six Go collectors
    (`context_engine_api_requests_total`,
    `_api_request_duration_seconds`, `_kafka_consumer_lag`,
    `_pipeline_stage_duration_seconds`,
    `_retrieval_backend_duration_seconds`,
    `_retrieval_backend_hits`) plus a per-prefix triplet
    (`<prefix>_requests_total`, `<prefix>_duration_seconds`,
    `<prefix>_queue_depth`) for each Python service. Four HPA
    manifests (`deploy/hpa-api.yaml`, `hpa-ingest.yaml`,
    `hpa-docling.yaml`, `hpa-embedding.yaml`) target the matching
    metrics with explicit scale-up / scale-down stabilization
    windows.
  - **Phase 8 Mem0 partitioning**:
    `services/memory/memory_server.py::tenant_prefix` keys every
    Mem0 operation by tenant prefix; metadata records
    `tenant_id` and `tenant_prefix`; search filters stray rows.
    Cross-tenant isolation verified by
    `services/memory/test_partitioning.py`; Go-side tenant_id
    propagation verified by `cmd/api/memory_adapter_test.go`.
  - **Phase 8 probes**: `/healthz` and `/readyz` on the API
    binary (`cmd/api/readyz.go`, checks Postgres + Redis +
    Qdrant) and the ingest binary (`cmd/ingest/health.go`,
    checks Postgres + Redis + every Kafka broker via
    `net.DialTimeout`); both binaries also serve `/metrics` from
    the same listener. Tests in `cmd/api/readyz_test.go` and
    `cmd/ingest/health_test.go` use `sqlmock` to drive the
    Postgres dependency.
  - **Phase 1/3 P95 budget enforcement**: new
    `tests/benchmark/p95_e2e_test.go::TestE2E_RetrieveP95`
    (build tag `e2e`) issues 200 retrieval requests through the
    full Gin handler stack with synthetic vector + BM25 + graph
    + memory backends and fails the test if P95 > 500 ms or
    round-trip P95 > 1 s. The new
    `RetrieveResponse.Timings` envelope breaks per-backend
    latency down (vector_ms, bm25_ms, graph_ms, memory_ms,
    merge_ms, rerank_ms) so operators can identify the long
    pole. Retrieval optimisations include
    `QdrantClient.Warmup` (parallel `GET /` to pre-establish
    the http.Transport pool on startup) and
    `FalkorDBClient.KeepAlive` (background ticker that pings
    `GRAPH.LIST` to keep the redis pool warm). Both are wired
    into `cmd/api/main.go` after the listener starts.
  - **Make targets**: `make test-connector-smoke` and
    `make bench-e2e` exposed.
- 2026-05-10: Phase 5 server-side (~40%) + Phase 7 catalog (~85%) +
  Phase 8 optimisation (~50%):
  - **Phase 5**: shard manifest API and metadata store
    (`internal/shard/`, `migrations/006_shards.sql`,
    `GET /v1/shards/:tenant_id`); policy-aware shard generation
    worker (`internal/shard/generator.go` calls
    `PolicyResolver.Resolve`); delta sync protocol
    (`GET /v1/shards/:tenant_id/delta?since=<v>`,
    `internal/shard/delta.go`); cryptographic forgetting
    orchestrator (`internal/shard/forget.go`,
    `DELETE /v1/tenants/:tenant_id/keys`).
  - **Phase 7**: 10 new connectors landed —
    `internal/connector/sharepoint`, `…/onedrive`, `…/dropbox`,
    `…/box`, `…/notion`, `…/confluence`, `…/jira`,
    `…/github`, `…/gitlab`, `…/teams`. Each implements
    `SourceConnector`; all 10 implement `DeltaSyncer`; Jira /
    GitHub / GitLab / Teams also implement `WebhookReceiver`.
    Total connector catalog: 12 (target hit at GA).
  - **Phase 8**: OpenTelemetry tracing helper
    (`internal/observability/tracing.go`) instrumenting the four
    pipeline stages (`pipeline.coordinator`) and the four
    retrieval backends (`retrieval.handler.fanOut`); `trace_id`
    echoed on `RetrieveResponse`. Per-stage worker pools
    (`pipeline.StageConfig` in `CoordinatorConfig`). Sticky
    Kafka rebalance + tunable session/poll/strategy
    (`pipeline.ConsumerTuning`, `SaramaConfigWith`). Storage
    pool sizing exposed via env (`CONTEXT_ENGINE_QDRANT_POOL_SIZE`,
    `CONTEXT_ENGINE_REDIS_POOL_SIZE`, `CONTEXT_ENGINE_PG_MAX_OPEN`,
    `CONTEXT_ENGINE_PG_MAX_IDLE`). gRPC pool with circuit
    breaker (`internal/grpcpool/`). Capacity test harness
    (`tests/capacity/`, `make capacity-test`).
- 2026-05-09: Phase 4 hardening (~98%): transactional audit on
  promotion / rejection — `policy.AuditWriter` now exposes
  `CreateInTx` and `internal/policy/promotion.go` emits the
  `policy.promoted` / `policy.rejected` audit row inside the outer
  `*gorm.DB` transaction, so a `LiveStore.ApplySnapshot` /
  `MarkPromoted` failure rolls the audit row back along with the
  rest. Live simulator wiring in `cmd/api`:
  `policy.NewLiveResolverGORM` (new
  `internal/policy/live_resolver.go`) reads the live policy tables
  from `migrations/004_policy.sql`, and the simulator's `Retriever`
  port now delegates to `retrieval.Handler.RetrieveWithSnapshot`
  (new method that runs the full fan-out + merge + rerank +
  ACL/recipient gate against an explicit snapshot, deliberately
  bypassing the cache). Phase 2/3/4 e2e coverage in
  `tests/e2e/phase234_test.go`: admin source CRUD,
  ACL-deny-drops-hits via `LiveResolverGORM` +
  `RetrieveWithSnapshot`, draft create/promote/reject through the
  HTTP surface with a roundtrip-via-Postgres audit assertion, and
  simulator endpoint smoke checks.
- 2026-05-09: Phase 4 simulator + drafts + privacy strip (~95%):
  policy what-if engine (`internal/policy/simulator.go`,
  copy-on-write `PolicySnapshot.Clone()`), data-flow diff with
  per-tier counts and percentage delta
  (`internal/policy/simulator_diff.go`), conflict detection for
  privacy-mode overrides, ACL overlaps, and recipient policy
  contradictions (`internal/policy/simulator_conflict.go`),
  GORM-backed draft store (`internal/policy/draft.go`,
  `migrations/005_policy_drafts.sql`) isolated from the live
  resolver, audited promotion workflow with `policy.promoted` /
  `policy.rejected` actions wired into `internal/audit/model.go`
  (`internal/policy/promotion.go`), GORM-backed live store
  applying snapshots transactionally to `tenant_policies`,
  `channel_policies`, `policy_acl_rules`, and `recipient_policies`
  (`internal/policy/live_store.go`), simulator + drafts HTTP
  surface mounted under `/v1/admin/policy/`
  (`internal/admin/simulator_handler.go`), privacy strip enrichment
  in retrieval responses (`internal/retrieval/privacy_strip.go`).
- 2026-05-09: Phase 2 partial (~100%) + Phase 4 partial (~30%):
  admin source-management API surface (`internal/admin/`,
  `migrations/002_sources.sql`, `migrations/003_source_health.sql`),
  per-tenant Kafka producer with partition-key routing
  (`internal/pipeline/producer.go`,
  `internal/pipeline/consumer.go::ParsePartitionKey`), backfill
  vs steady-state pipeline orchestrator with paced rate control
  (`internal/pipeline/backfill.go`, `IngestEvent.SyncMode`), per-source
  Redis token-bucket rate limiter (`internal/admin/ratelimit.go`),
  source-health tracking (`internal/admin/health.go`,
  `migrations/003_source_health.sql`), forget-on-removal worker with
  fenced lease (`internal/admin/forget_worker.go`), connector
  lifecycle audit actions (`internal/audit/model.go`), policy
  framework (`internal/policy/privacy_mode.go`,
  `internal/policy/acl.go`, `internal/policy/recipient.go`,
  `migrations/004_policy.sql`) wired into the retrieval handler via
  `internal/retrieval/policy_snapshot.go`.
- 2026-05-09: Phase 3 partial — Go retrieval API completion (BM25 via
  bleve, FalkorDB graph traversal, Redis semantic cache,
  RRF merger + lightweight reranker, parallel fan-out with per-backend
  deadlines, `policy.degraded` signalling), Python ML microservices
  (Docling, embedding, Mem0) with proto stubs and unit tests,
  Go ↔ Python integration tests, throughput / latency benchmarks
  in `tests/benchmark/`, cutover plan in `docs/CUTOVER.md`.
- 2026-05-09: Phase 1 complete — Google Drive + Slack connectors, Go
  Kafka consumer, 4-stage pipeline (fetch / parse / embed / store),
  retrieval API (`POST /v1/retrieve`), CI smoke test against docker
  compose storage plane.
- 2026-05-09: Phase 0 complete — `SourceConnector` interface, registry,
  credential encryption, audit log (table + outbox + API), gRPC proto
  definitions for Docling/Embedding/Mem0.
