# hunting-fishball — Progress

This document tracks the *actual* state of the platform. The shape mirrors
[`PHASES.md`](PHASES.md). Anything not marked here is `⏳ planned`.

| Status legend |  |
|---------------|--|
| ✅ shipped | Landed in `main` and exercised in production |
| 🟡 partial | Some criteria met; gaps listed inline |
| ⏳ planned | Not yet started |

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

**Status.** ✅ shipped | ~100% — **54** production connectors in the catalog as of Round 24 (Quip, Freshservice, PagerDuty, Zoho Desk added on top of the Round-20/21 fifty). Per-connector runbooks under `docs/runbooks/`, end-to-end smoke green per connector (`tests/e2e/connector_smoke_test.go`, `make test-connector-smoke`), capability matrix below.

- [x] ≥ 12 production connectors at GA — the catalog now ships
      **54** connectors spanning chat (Slack, KChat, Mattermost,
      Discord, Teams, Webex), docs / wiki (Google Drive, Google
      Shared Drives, SharePoint Online + on-prem, OneDrive,
      Dropbox, Box, Notion, Confluence Cloud + Server/DC, Quip,
      Coda, BookStack), issue / VCS (Jira, GitHub, GitLab,
      Bitbucket, Linear, Asana, ClickUp, Monday.com, Trello),
      CRM (Salesforce, HubSpot, Pipedrive), identity (Okta,
      Entra ID, Google Workspace Directory), HR (Workday,
      BambooHR, Personio), email (Gmail, Outlook), support / ITSM
      (Zendesk, ServiceNow, Freshdesk, Freshservice, Intercom,
      Zoho Desk, PagerDuty), object storage (S3, Azure Blob,
      GCS, Egnyte), generic web (RSS / Atom, Sitemap), and a
      signed-upload portal. Each lives under
      `internal/connector/<name>/`.
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
| Quip | ✅ (`/1/users/current`) | ✅ (threads via `/1/threads/recent`) | ❌ | ✅ (`updated_usec` microsecond cursor) | ❌ | ✅ Round-24 (Phase 2+, Docs) |
| Freshservice | ✅ (`/api/v2/agents/me`) | ✅ (tickets via `/api/v2/tickets`) | ❌ | ✅ (`updated_since=<ISO8601>` + page-based) | ❌ | ✅ Round-24 (Phase 2+, ITSM) |
| PagerDuty | ✅ (`/users/me` user-token / `/abilities` rest-key) | ✅ (incidents via `/incidents`) | ❌ (planned) | ✅ (`since=<ISO8601>` + `more`/`offset`) | ❌ | ✅ Round-24 (Phase 2+, Incident) |
| Zoho Desk | ✅ (`/api/v1/myinfo` + `orgId`) | ✅ (tickets via `/api/v1/tickets`) | ❌ | ✅ (`modifiedTimeRange=<since,until>` + 100/page) | ❌ | ✅ Round-24 (Phase 2+, Support) |

---

## Changelog

- 2026-05-13: **Round 24: Connector catalog 50 → 54 + production-
  hardening + doc cleanup.** Tasks 1-4 ship four new connectors —
  `quip` (Salesforce-owned docs, Bearer + `updated_usec`
  microsecond cursor), `freshservice` (Freshworks ITSM, API
  key basic-auth user with password `X`, `updated_since=<ISO8601>`
  + page-based pagination matching Round-22's Freshdesk
  `Link: rel="next"` fix), `pagerduty` (incident knowledge,
  `Authorization: Token token=...` scheme with both user and
  REST-key token shapes, `since=<ISO8601>` + `more`/`offset`
  pagination), and `zoho_desk` (Zoho Desk tickets, OAuth
  `Authorization: Zoho-oauthtoken` + `orgId` header,
  `modifiedTimeRange=<since,until>` with 100/page ceiling).
  Each connector ships full `Validate` / `Connect` /
  `ListNamespaces` / `ListDocuments` / `FetchDocument` /
  `DeltaSync` httptest coverage, the empty-cursor bootstrap
  contract that captures `now()` without backfilling, the
  ErrRateLimited 429 wrap, ErrInvalidConfig / ErrNotSupported
  references, and blank-imports in `cmd/api/main.go` +
  `cmd/ingest/main.go`. Task 5 lifts `audit_test.go` floor
  49 → 53; task 6 lifts `connector_smoke_test.go` registry
  pin 50 → 54 with full-lifecycle smokes for all four; task 7
  ships `tests/e2e/round24_test.go` (build tag `e2e`) with
  the 54-connector registry pin, a Quip lifecycle walk, and
  a 429-propagation sweep across the new four; task 8 ships
  `tests/regression/round2223_manifest.{go,_test.go}` pinning
  every Round-22/23 Devin Review fix (Airtable cursor
  seconds + microseconds, Freshdesk `Link: rel="next"`
  pagination, Qdrant healthcheck validation, ConnectorHealthHandler
  aggregation, Intercom namespace branching, paused-source
  filter); task 9 adds compile-time `SourceConnector` +
  `DeltaSyncer` assertions to `tests/integration/connector_contract_test.go`;
  task 10 adds credential-decode fuzzers for all four
  connectors and registers them with `make fuzz`. Task 11
  adds a `by_connector_category` nested rollup to
  `/v1/admin/dlq/analytics` so on-call can pin both the
  noisy connector and its dominant failure class without a
  second query. Task 12 adds a `ConsecutiveFailureThreshold`
  trigger to the source auto-pauser (config knob
  `CONTEXT_ENGINE_SOURCE_AUTO_PAUSE_THRESHOLD`) so hard-down
  upstreams can be paused without waiting for the sliding-
  window MinSampleSize to fill. Task 13 ships two
  self-documenting alias routes on top of the existing
  bulk-source pipeline — `POST /v1/admin/sources/bulk-reindex`
  and `POST /v1/admin/sources/bulk-rotate`. Task 15 adds a
  `content_type` field to `IngestEvent` for future
  multimodal routing (image/audio/video) without breaking
  the existing JSON shape (omitempty preserves byte-level
  compatibility). Task 20 adds four per-connector runbooks
  under `docs/runbooks/` covering credential rotation,
  quota / rate-limit handling, outage detection, and error
  codes; task 21 lifts `runbook_test.go` floor 50 → 54 and
  blank-imports the four new packages. Task 22 documents the
  bulk-reindex / bulk-rotate / analytics-connector-health /
  webhooks-billing surfaces in `docs/openapi.yaml`. Task 24
  stands up `POST /v1/admin/webhooks/billing` (plus GET and
  DELETE) with HTTPS-only URL validation, 32-char minimum
  shared-secret enforcement, an audit-logged lifecycle, and a
  fakeable `BillingWebhookStore` seam — the emit path lands
  in a future round once the billing team has the storage
  schema. Task 25 mounts `GET /v1/admin/analytics/connector-health`
  with a `window` query parameter (1h / 24h / 7d / 30d); the
  payload today is a point-in-time snapshot, but the response
  shape is locked in so a future time-series backend can be
  swapped behind it without breaking clients. Tasks 26-30
  rewrite the README as a public-facing project doc, trim
  PROGRESS / PHASES / ARCHITECTURE of the round-by-round
  status snapshots that had grown to look like an internal
  task tracker, and align the Phase-2+ table footnote in
  PROPOSAL with the now-extended catalog. CI invariants:
  `fast-required` aggregator pinned green; `audit_test.go`,
  `bootstrap_audit_test.go`, `connector_smoke_test.go`, and
  `runbook_test.go` floors moved in lockstep with the 50 → 54
  registry growth.
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
- 2026-05-12: **Round 16**: Connector catalog 20 → 28 (Mattermost, ClickUp, Monday.com, Pipedrive, Okta, Gmail, RSS/Atom, Confluence Server/DC). Audit/smoke/runbook gates lifted to 28. Two new fast-lane CI jobs (`fast-connector-integration`, `fast-regression`). 8 new runbooks.
- 2026-05-12: **Round 15**: Connector catalog 12 → 20 (KChat, S3-compatible, Linear, Asana, Discord, Salesforce, HubSpot, Google Shared Drives). `connector.ErrRateLimited` sentinel + process-global completeness audit (`internal/connector/audit_test.go`). `fast-connector-unit` CI lane. 7 new runbooks.
- 2026-05-12: **Round 14**: Observability + CI hardening — slow-query persistence (`migrations/038_slow_queries.sql`), per-tenant payload caps (`039_tenant_payload_limits.sql`), DLQ categorisation with auto-replay (`040_dlq_category.sql`), pipeline stage breaker dashboards, CI fast lane split (`fast-check` / `fast-test` / `fast-build`).
- 2026-05-11: **Round 13**: SLO + health surface — `GET /v1/admin/health/summary`, gRPC health checks on sidecars, new PrometheusRule entries for chunk-quality / cache-hit / credential health, `make doctor` developer-prereq script, per-stage pipeline breakers, retrieval health summary.
- 2026-05-11: **Round 12**: DLQ admin + retention + cardinality — DLQ replay tooling, audit retention worker, cursor-aware pagination across audit/admin handlers, Prometheus cardinality policy (`TestMetrics_NoTenantIDLabel`).
- 2026-05-11: **Round 11**: Fuzz fixes, graceful-degradation timeouts on GORM stores (`gracefulLookupTimeout=200ms`), Stage-4 hook timeout guard (`CONTEXT_ENGINE_HOOK_TIMEOUT`), batch retrieval diversity, migration-ordering invariants (`migrations/migration_order_test.go`), per-PR concurrency cancellation in CI.
- 2026-05-11: **Round 10**: Hook framework wiring + e2e for Phase 4 simulator, chunk-quality scoring hook (`CONTEXT_ENGINE_CHUNK_SCORING_ENABLED`), synonym-driven BM25 expansion (`CONTEXT_ENGINE_QUERY_EXPANSION_ENABLED`).
- 2026-05-11: **Round 9**: Finished the GORM cutover for latency-budget / cache-TTL / pin-list / query-analytics / sync-history / chunk-quality stores, recording rules + pool gauges, cross-backend dedup + cache warm-on-miss, OpenAPI completeness pass.
- 2026-05-11: **Round 8**: Pipeline wiring (priority buffer / dedup), GORM-backed notification subscriptions, end-to-end SSE streaming (`CONTEXT_ENGINE_SSE_STREAMING_ENABLED`), capacity harness (`tests/capacity/`), new runbooks.
- 2026-05-11: **Round 7**: Query analytics, retrieval cache warming + pinning, GORM stores for synonyms / connector templates / latency budgets, retrieval diversity + per-chunk ACL gating (`CONTEXT_ENGINE_CHUNK_ACL_ENABLED`).
- 2026-05-10: **Round 6**: Retrieval diversity, semantic dedup at Stage 4 (`CONTEXT_ENGINE_DEDUP_ENABLED`), priority buffer for hot-tenant fairness (`CONTEXT_ENGINE_PRIORITY_BUFFER_ENABLED`), ACL snapshot resolution at retrieval time.
- 2026-05-10: **Round 5**: CI fix + next-20 hardening pass — fast-lane stabilisation, eval-runner coverage, GraphRAG entity contract.
- 2026-05-10: **Round 4**: Eval runner, GraphRAG proto + sidecar (`proto/graphrag/v1/`, `services/graphrag/`), sync schedules table (`013_sync_schedules.sql`), tenant usage rollup (`014_tenant_usage.sql`), retrieval evaluation harness (`internal/eval/`), per-migration rollback set under `migrations/rollback/`.
- 2026-05-10: Phase 8 production-hardening sweep — DLQ admin surface (`/v1/admin/dlq*` + `migrations/009_dlq_messages.sql`), retention worker (`migrations/010_retention_policy.sql`), reindex pipeline, per-tenant API rate limiter, webhook HMAC verifier, graceful shutdown, startup config validation, bulk retrieval (`POST /v1/retrieve/batch`), audit search, sync progress, full OpenAPI spec.
- 2026-05-10: Phase 5/6 closeout — shard manifest + delta + coverage endpoints, cryptographic-forgetting orchestrator, B2C client SDK bootstrap, on-device contracts (UniFFI iOS/Android, NAPI desktop), benchmarks + e2e suites.
- 2026-05-10: Phase 5 contracts → ~70% + Phase 6 server-side bootstrap + Phase 8 on-device contracts.
- 2026-05-10: Phase 7 catalog → ~100% + Phase 8 optimisation + retrieval P95 budget enforced + liveness/readiness probes.
- 2026-05-10: Phase 5 server-side + Phase 7 catalog expansion + Phase 8 optimisation pass.
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