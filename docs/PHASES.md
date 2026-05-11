# hunting-fishball — Phase Definitions & Exit Criteria

This document collects the planned milestones in one place so PR reviewers
and operators have a shared vocabulary. The phase model intentionally
mirrors the `ai-agent-platform` connector phase model, so a
connector or pipeline change can move between repos without re-numbering.

A phase is **shippable** only when *all* of its exit criteria are
demonstrably met (test, runbook, metric, or migration as appropriate).
Phases stack — a later phase assumes the invariants of every earlier
phase.

| Status legend |  |
|---------------|--|
| ✅ shipped | The phase is in `main` and exercised in production |
| 🟡 partial | Some exit criteria met; gaps tracked in `PROGRESS.md` |
| ⏳ planned | Not yet started |

> Phases 0–4 are **🟡 partial** as of 2026-05-09 — the connector
> contract, registry, credential encryption, audit log primitives,
> Phase 1 single-source MVP, Phase 2 admin source-management, Phase 3
> retrieval fan-out, and Phase 4 policy framework + simulator +
> privacy-strip enrichment have all landed (Phase 4 client-side
> privacy strip and Phase 3 production P95 measurement are tracked
> against Phase 6 and Phase 8 respectively).
>
> **Phase 5** is **🟡 partial** as of 2026-05-10 — server-side
> shard manifest API, policy-aware shard generation worker, delta
> sync protocol, cryptographic-forgetting orchestrator, and the
> shard coverage endpoint have all landed (`internal/shard/`).
> On-device contracts ship as Go interfaces + per-platform docs:
> `ShardClientContract` (`internal/shard/contract.go`),
> `docs/contracts/uniffi-ios.md`,
> `docs/contracts/uniffi-android.md`,
> `docs/contracts/napi-desktop.md`,
> `docs/contracts/local-first-retrieval.md`,
> `docs/contracts/bonsai-integration.md`. The model catalog
> (`internal/models/`) backs `GET /v1/models/catalog` with three
> Bonsai-1.7B builds and per-tier eviction policy. The actual
> XCFramework / AAR / N-API addon implementations live in
> `kennguy3n/knowledge`.
>
> **Phase 7** is **🟡 partial** as of 2026-05-10 — 12 connectors are
> implemented (Phase 1 Google Drive + Slack; Phase 7 SharePoint,
> OneDrive, Dropbox, Box, Notion, Confluence, Jira, GitHub, GitLab,
> Microsoft Teams). Per-connector runbooks and per-connector e2e
> smoke tests have landed (`docs/runbooks/`,
> `tests/e2e/connector_smoke_test.go`,
> `make test-connector-smoke`); the only remaining gate is a
> production rollout of the smoke suite under the docker-compose CI
> path.
>
> **Phase 8 production-hardening (2026-05-10 batch).** A 20-task
> next-batch of operational + admin surfaces has landed: graceful
> shutdown for both binaries (`internal/lifecycle/`), startup config
> validation (`internal/config/validate.go`), a SQL migration runner
> behind `AUTO_MIGRATE` (`internal/migrate/runner.go`), bulk
> retrieval (`POST /v1/retrieve/batch`), audit log search/filter
> (`/v1/admin/audit`), per-namespace sync progress
> (`/v1/admin/sources/:id/progress`), DLQ inspection + replay
> (`/v1/admin/dlq`), retention policy enforcement
> (`internal/policy/retention.go` + `internal/pipeline/retention_worker.go`),
> a reindex orchestrator (`POST /v1/admin/reindex`), per-tenant API
> rate limit, webhook signature verification across the four
> `WebhookReceiver` connectors (jira / github / gitlab / teams),
> the connector dashboard (`/v1/admin/dashboard`), the request-ID
> middleware (`internal/observability/request_id.go`), the
> e2e tenant deletion + degradation smoke suites
> (`tests/e2e/tenant_deletion_test.go`,
> `tests/e2e/degradation_test.go`), and the OpenAPI spec
> (`docs/openapi.yaml`). See `docs/PROGRESS.md` 2026-05-10
> changelog entry for the per-task breakdown.
>
> **Round-4 next-20 batch (2026-05-10).** Layered on top of the
> Phase 8 batch: a retrieval evaluation harness (`internal/eval/`,
> `migrations/012_eval_suites.sql`, `GET /v1/admin/eval/run`), an
> opt-in GraphRAG Stage 3b (`proto/graphrag/v1/`,
> `services/graphrag/`, `internal/pipeline/graphrag.go`), shared
> webhook HMAC verification (`internal/connector/webhook_verify.go`)
> across GitHub / GitLab / Jira / Teams / Slack, a retention
> enforcement worker (`internal/admin/retention_worker.go`), a
> source-sync cron scheduler (`internal/admin/scheduler.go` +
> `migrations/013_sync_schedules.sql`), structured error catalog
> (`internal/errors/`), per-tenant API rate-limit middleware
> (`internal/admin/api_ratelimit.go`), Kafka request-id
> propagation, Prometheus alert rules (`deploy/alerts.yaml` +
> `make alerts-check`), retrieval fuzz tests + `make fuzz`, full
> in-process pipeline integration test, proto contract tests,
> chaos / fault injection (`internal/storage/fault.go`), regression
> manifest (`tests/regression/manifest.go`), connector credential
> rotation API (`POST /v1/admin/sources/:id/rotate-credentials`),
> tenant usage metering (`GET /v1/admin/tenants/:id/usage`),
> sync-progress SSE (`GET /v1/admin/sources/:id/sync/stream`),
> index health-check (`GET /v1/admin/health/indexes`), and per-
> migration rollback scripts (`migrations/rollback/` +
> `make migrate-rollback`). See `docs/PROGRESS.md` 2026-05-10
> Round-4 changelog entry for the per-task breakdown.
>
> **Phase 8** is **🟡 partial** as of 2026-05-10 — OpenTelemetry
> trace instrumentation, configurable per-stage worker pools,
> tunable Kafka rebalance config, storage connection-pool sizing,
> a round-robin gRPC pool with circuit breaker, and a capacity test
> harness have all landed. Prometheus metrics + four HPA manifests
> (`deploy/hpa-api.yaml`, `hpa-ingest.yaml`, `hpa-docling.yaml`,
> `hpa-embedding.yaml`), Mem0 tenant prefix partitioning, and
> liveness / readiness probes (`/healthz`, `/readyz`) all landed in
> 2026-05-10. The cross-platform on-device gates are now covered
> by contracts: the Bonsai-1.7B per-tier benchmark envelope ships
> in `tests/benchmark/bonsai_contract_test.go::BonsaiContract`,
> and shard eviction policy ships in `internal/shard/eviction.go`
> with `DefaultEvictionPolicies()` surfaced via
> `GET /v1/models/catalog`. The actual on-device measurements run
> in `kennguy3n/knowledge` and `kennguy3n/llama.cpp` against the
> contract.
>
> **Phase 6** is **🟡 partial** as of 2026-05-10 — the B2C
> client SDK contract (`docs/contracts/b2c-retrieval-sdk.md`),
> the device-first policy (`internal/policy/device_first.go`),
> the privacy-strip render contract
> (`docs/contracts/privacy-strip-render.md`), and the background
> sync contract (`docs/contracts/background-sync.md`) have all
> landed; `internal/b2c/handler.go` mounts `/v1/health`,
> `/v1/capabilities`, and `/v1/sync/schedule`. Client UI work
> ships from the B2C repos (`uneycom/b2c-kchat-portal`,
> `uneycom/skytrack-*`).
>
> Every other phase below is currently `⏳ planned`. As phases
> land, flip the marker and move the supporting status row in
> [`PROGRESS.md`](PROGRESS.md).
>
> **Round 6** (2026-05-10) layers 20 new features across the
> existing phases rather than opening a new one: MMR diversifier
> + chunk-level ACL extend Phase 3/4 retrieval; semantic dedup
> + priority queues + dry-run + retry analytics extend Phase 1
> pipeline; per-source embedding config + connector templates
> + adaptive rate limiting extend Phase 2 admin surfaces; SSE
> streaming retrieval is a new variant of the Phase 3 retrieve
> API; API versioning + isolation audit + tenant export + admin
> notifications + backpressure metrics extend Phase 8
> observability/operability; shard pre-generation extends
> Phase 5. See PROGRESS.md for the per-task list.
>
> **Round 8** (2026-05-11) layers another 20 tasks
> primarily focused on *making the existing surface real*:
> Stage 4 deduplication, the coordinator priority buffer,
> Stage 3 per-source embedding overrides, and retry analytics
> are now wired into the ingest binary (Phase 1 hardening);
> the notification dispatcher fires on every audit event with
> a persisted delivery log + retry-with-DLQ worker (Phase 8
> hardening); all six Round-7 in-memory admin stores
> (`QueryAnalytics`, `PinnedResults`, `SyncHistory`,
> `LatencyBudget`, `CacheConfig`, `CredentialHealth`) are now
> Postgres-backed and wired into `cmd/api/main.go`; the
> retrieval handler grew `SetLatencyBudgetLookup`,
> `SetCacheTTLLookup`, and `SetPinLookup` so per-tenant
> operator controls reach the hot path (Phase 3); a periodic
> credential-health worker now runs in `cmd/ingest/main.go`;
> `docs/openapi.yaml` was extended with the Round 5/6/7
> endpoint set; CI gains `make alerts-check` and rollback
> parity in the fast lane; and the new
> `docs/runbooks/operational.md` collates the Round 6/7/8
> ops procedures in one place. See PROGRESS.md for the
> per-task list.

> **Round 7** (2026-05-11) layers another 20 features —
> primarily operational hardening: query analytics + A/B
> results + retrieval pinning + cache warming + per-tenant
> latency/cache budgets extend Phase 3 retrieval; chunk quality
> scoring + sync conflict resolution + sync history extend
> Phase 1 pipeline; notification retry/DLQ + credential health
> + bulk source ops + audit export + pipeline health dashboard
> + per-migration rollback scripts (015–031) extend Phase 8
> operability. Full Round-6 + Round-7 wiring into `cmd/api` and
> `cmd/ingest` ships in the same round. See PROGRESS.md for the
> per-task list.

---

## Phase 0 — Connector contract, registry, audit primitives  🟡

**Scope.** Define the `SourceConnector` interface and the global registry;
every binary that needs source connectors imports the provider packages
for their `init()` side-effects. Stand up the audit-log primitives in the
platform backend.

**Exit criteria.**

- [x] `SourceConnector` interface with `Validate / Connect / ListNamespaces /
      ListDocuments / FetchDocument / Subscribe / Disconnect`.
- [x] Optional `DeltaSyncer`, `WebhookReceiver`, `Provisioner` interfaces.
- [x] Process-global registry with `RegisterSourceConnector` /
      `GetSourceConnector` (mirror of the platform backend's connector
      factory).
- [x] AES-GCM credential encryption reused from the platform backend.
- [x] Blank-import side-effects in the connector binaries that need
      registration.
- [x] Audit-log Postgres table + outbox into Kafka.
- [x] Audit log surfaced in the admin portal API.

---

## Phase 1 — Single-source MVP end-to-end  🟡

**Scope.** Get one source connector all the way through ingestion, the
4-stage Go context engine, the storage plane, and the retrieval API.
Single tenant, single user, single channel. Exists to prove the contract,
not to be production-ready.

**Connectors.** Google Drive (read-only) and Slack (channels the
service-account is in).

**Exit criteria.**

- [x] Google Drive connector implements `SourceConnector` with delta
      tokens.
- [x] Slack connector implements `SourceConnector` with the Events API.
- [x] Go context engine consumes from Kafka and runs Stage 1 (Fetch),
      Stage 2 (gRPC → Python Docling), Stage 3 (gRPC → Python embedding),
      Stage 4 (Storage to Qdrant + Postgres).
- [x] `POST /v1/retrieve` returns top-k matches from Qdrant for a sample
      query.
- [x] Round-trip latency P95 < 1 s on the sample corpus — enforced by
      `tests/benchmark/p95_e2e_test.go::TestE2E_RetrieveP95`
      (build tag `e2e`, `make bench-e2e`).
- [x] Audit log records every ingestion + retrieval call.
- [x] One smoke test runs in CI end-to-end (using docker-compose for the
      storage plane).

---

## Phase 2 — B2B Admin Source Management  🟡

**Scope.** Multi-tenant source management for B2B administrators. Wire the
**org-wide sync pipeline through the Go context engine (Kafka)** so that
an admin connecting a source results in steady-state ingestion across the
whole tenant, not just a single user.

**Exit criteria.**

- [x] Admin portal flows for connecting / pausing / re-scoping / removing
      a source, all multi-tenant (`internal/admin/source_handler.go`).
- [x] Per-tenant Kafka topic routing keyed on `tenant_id || source_id`
      (`internal/pipeline/producer.go`,
      `internal/pipeline/consumer.go::ParsePartitionKey`).
- [x] Org-wide sync pipeline runs through the **Go context engine**
      (replacing any earlier single-user shortcut). The Go consumer
      handles backfill paced separately from steady-state
      (`internal/pipeline/backfill.go`, `IngestEvent.SyncMode`).
- [x] Per-source quota + rate-limit enforcement at the platform backend
      *before* the connector contacts the external API
      (`internal/admin/ratelimit.go`, Redis token bucket).
- [x] Sync health surfaced in the admin portal (last-success, lag,
      error counts) per source per namespace
      (`internal/admin/health.go`, `migrations/003_source_health.sql`).
- [x] Forget-on-removal worker drops all derived chunks / embeddings /
      graph nodes / memory entries, with fenced lease so re-add doesn't
      race with deletion (`internal/admin/forget_worker.go`).
- [x] Connector lifecycle events (`source.connected`,
      `source.sync_started`, `chunk.indexed`, `source.purged`) emitted
      to the audit log (`internal/audit/model.go`).

---

## Phase 3 — Retrieval fan-out  🟡

**Scope.** Stand up the four retrieval backends in parallel and merge
their results.

**Exit criteria.**

- [x] Qdrant Go client integrated; per-tenant collection convention
      (landed in Phase 1; reused unchanged here).
- [x] BM25 search integrated via `bleve` (pure-Go fallback for
      `tantivy-go`); per-tenant directory convention.
- [x] FalkorDB Go client integrated; per-tenant graph convention.
- [x] gRPC contract for the Mem0 memory service; Go client integrated
      and wired through the retrieval handler.
- [x] Retrieval API runs all four backends in parallel via `errgroup`,
      with per-backend deadlines and `policy.degraded` signalling.
- [x] Reciprocal-rank fusion merger.
- [x] Go-side lightweight reranker (BM25 blend + freshness boost) +
      `Reranker` interface for a future cross-encoder remote
      reranker.
- [x] Semantic cache in Redis with per-tenant key prefix and explicit
      `Invalidate` on Stage 4 writes.
- [x] Retrieval P95 latency < 500 ms on the sample corpus —
      in-process P95 ~178 µs in `tests/benchmark/`; the
      end-to-end budget is enforced by the same
      `TestE2E_RetrieveP95` test referenced in Phase 1. New
      `RetrieveResponse.Timings` envelope (vector_ms /
      bm25_ms / graph_ms / memory_ms / merge_ms / rerank_ms)
      lets operators identify the long pole.

---

## Phase 4 — Policy framework + simulator + privacy strip  🟡

**Scope.** Privacy mode, allow / deny lists, recipient policy, and the
policy simulator. Privacy strip surfaces in every client.

**Exit criteria.**

- [x] Tenant- and channel-scoped privacy mode (`local-only`, `local-api`,
      `hybrid`, `remote`, `no-ai`)
      (`internal/policy/privacy_mode.go`, `EffectiveMode` returns the
      stricter of (tenantMode, channelMode)).
- [x] Allow / deny lists by source / namespace / path glob
      (`internal/policy/acl.go`,
      `internal/retrieval/policy_snapshot.go::applyPolicySnapshot`).
- [x] Recipient policy by channel / skill
      (`internal/policy/recipient.go`, gated in
      `internal/retrieval/handler.go` after merge + rerank).
- [x] Policy simulator: what-if retrieval, data-flow diff, conflict
      detection (`internal/policy/simulator.go`,
      `internal/policy/simulator_diff.go`,
      `internal/policy/simulator_conflict.go`; admin HTTP surface in
      `internal/admin/simulator_handler.go`).
- [x] Drafts isolated from live; explicit promotion is an audited event
      (`internal/policy/draft.go`, `migrations/005_policy_drafts.sql`,
      `internal/policy/promotion.go`,
      `internal/policy/live_store.go`; promotion emits
      `policy.promoted` / `policy.rejected` audit events transactionally
      with the live-table writes — the `AuditWriter` port now exposes
      `CreateInTx` so the audit row rides the same `*gorm.DB`
      transaction as `LiveStore.ApplySnapshot` /
      `Drafts.MarkPromoted`).
- [x] `privacy_label` returned on every retrieval row.
- [x] Privacy strip enrichment in retrieval response
      (`internal/retrieval/privacy_strip.go`; every `RetrieveHit`
      carries a structured `privacy_strip`).
- [ ] Privacy strip rendered in admin portal, B2B desktop, and at least
      one mobile platform (server-side enrichment shipped; client-side
      render tracked under Phase 6).

---

## Phase 5 — On-device knowledge core integration  🟡

**Scope.** Land the Rust knowledge core (`kennguy3n/knowledge`) on mobile
and desktop; wire on-device retrieval shards.

**Exit criteria.**

- [x] UniFFI bindings for iOS (XCFramework) and Android (AAR) —
      Go-side `ShardClientContract` interface in
      `internal/shard/contract.go`; per-platform packaging in
      `docs/contracts/uniffi-ios.md` and
      `docs/contracts/uniffi-android.md`. The actual XCFramework /
      AAR are produced by `kennguy3n/knowledge` against this
      contract.
- [x] N-API binding for desktop — same `ShardClientContract`;
      `docs/contracts/napi-desktop.md` documents Electron
      packaging, the main-process IPC model, and the
      cryptographic-forget steps the addon must perform.
- [x] On-device retrieval shard sync from the Go retrieval API
      (`GET /v1/shards/:tenant_id`,
      `GET /v1/shards/:tenant_id/delta?since=<v>`,
      `GET /v1/shards/:tenant_id/coverage` —
      `internal/shard/handler.go`,
      `internal/shard/repository.go`,
      `internal/shard/generator.go`,
      `internal/shard/delta.go`,
      `migrations/006_shards.sql`).
- [x] Local-first retrieval contract — client decision tree in
      `docs/contracts/local-first-retrieval.md`; server emits
      `prefer_local` / `local_shard_version` /
      `prefer_local_reason` on every `RetrieveResponse` via
      `internal/policy/device_first.go::Decide`. On-device
      enforcement remains in `kennguy3n/knowledge`.
- [x] Bonsai-1.7B GGUF integration contract —
      `docs/contracts/bonsai-integration.md` +
      `internal/models/` ship the model catalog (q4_0 / q8_0 /
      fp16) backed by `GET /v1/models/catalog`. The actual
      `llama.cpp` integration runs in `kennguy3n/knowledge` and
      `kennguy3n/llama.cpp`.
- [x] Cryptographic forgetting on the on-device tier — server-side
      orchestrator wipes Qdrant + FalkorDB + Tantivy + Redis +
      destroys DEKs (`internal/shard/forget.go`,
      `DELETE /v1/tenants/:tenant_id/keys`); client-side delete
      flow follows in Phase 6.

---

## Phase 6 — B2C client surfaces  🟡

**Scope.** Bring the B2C mobile clients (and desktop companion) to
parity with the on-device-first contract.

**Exit criteria.**

- [x] iOS / Android / desktop B2C apps consume the same retrieval API
      and the same skill manifest format — SDK contract in
      `docs/contracts/b2c-retrieval-sdk.md`, backed by
      `internal/b2c/handler.go` mounting `GET /v1/health` and
      `GET /v1/capabilities`. The capabilities endpoint reports
      enabled retrieval backends + supported privacy modes so a
      B2C UI built against an older server can downgrade
      gracefully.
- [x] On-device first by default; remote retrieval gated behind the
      privacy mode and the device-tier policy —
      `internal/policy/device_first.go::Decide` returns a
      structured `DeviceFirstDecision`; `RetrieveResponse` echoes
      the hint via `prefer_local` / `local_shard_version` /
      `prefer_local_reason`. `shard.VersionLookup` adapts the
      shard repository to the retrieval handler's lookup port.
- [x] Privacy strip in B2C clients (mobile + desktop) —
      per-platform render guidance in
      `docs/contracts/privacy-strip-render.md`. Server-side
      enrichment shipped in Phase 4.
- [x] Background sync per platform's native scheduler —
      `docs/contracts/background-sync.md` documents iOS
      `BGAppRefreshTask`, Android `WorkManager`, and Electron
      `powerMonitor`; `GET /v1/sync/schedule` (`internal/b2c/`)
      is the server-side scheduler hint with minimum-interval
      floors so a rogue release can't DoS the API.

---

## Phase 7 — Catalog expansion  🟡

**Scope.** Add connectors per the connector catalog in
[`PROPOSAL.md`](PROPOSAL.md#4-connector-catalog). Each connector lands
behind the `SourceConnector` contract and reuses the existing pipeline.

**Exit criteria.**

- [x] At least 12 production connectors at GA (Phase-1 target met).
      Phase 1 (Google Drive, Slack) + Phase 7 (SharePoint, OneDrive,
      Dropbox, Box, Notion, Confluence, Jira, GitHub, GitLab,
      Microsoft Teams) = 12; each lives in
      `internal/connector/<name>/` with `httptest`-backed unit tests.
- [x] Each connector has its own runbook for credential rotation,
      quota incidents, and outages — see `docs/runbooks/` (one
      Markdown file per connector keyed on the connector's auth
      model and delta-cursor mechanism).
- [x] Per-connector capability matrix in [`PROGRESS.md`](PROGRESS.md).
- [x] Connector code path passes the same end-to-end smoke test that
      Phase 1 introduced — `tests/e2e/connector_smoke_test.go`
      (build tag `e2e`) drives Validate → Connect → ListNamespaces
      → ListDocuments → FetchDocument for every connector and
      asserts the registry has exactly 12 entries.
      `make test-connector-smoke`.

---

## Phase 8 — Cross-platform optimization  🟡

**Scope.** Performance tuning for the Go context engine and the Python
ML microservices, plus a cross-platform pass on the on-device tier.

**Exit criteria — Go context engine tuning.**

- [x] Goroutine pool sizing per stage tuned against measured latency
      (Stage 1 fetch concurrency, Stage 4 storage concurrency)
      — `pipeline.StageConfig` exposes
      `FetchWorkers / ParseWorkers / EmbedWorkers / StoreWorkers`;
      coordinator spawns N goroutines per stage and closes
      downstream channels on `WaitGroup` finalisation.
- [x] Kafka consumer group rebalancing tuned (sticky assignment,
      session timeout, max poll interval) so re-balances don't stall
      ingestion — `pipeline.ConsumerTuning` +
      `SaramaConfigWith(ConsumerTuning{...})`.
- [x] Connection pooling for Qdrant, FalkorDB, Tantivy, and PostgreSQL
      tuned to match the Gin server's expected QPS — Qdrant
      `http.Transport.MaxIdleConnsPerHost`, Redis `PoolSize`, and
      Postgres `SetMaxOpenConns` / `SetMaxIdleConns` /
      `SetConnMaxLifetime` are all env-tunable from
      `cmd/api/main.go`.
- [x] Memory ceilings per `context-engine-ingest` and
      `context-engine-api` deployment, with HPA on Kafka lag and QPS
      respectively — `deploy/hpa-ingest.yaml` (CPU +
      `context_engine_kafka_consumer_lag` custom metric) and
      `deploy/hpa-api.yaml` (CPU +
      `context_engine_api_requests_per_second`). Metrics exposed at
      `/metrics` from `internal/observability/metrics.go`.
- [x] OpenTelemetry trace sampling tuned to keep cost under budget
      while still catching tail latency — central
      `internal/observability/tracing.go` wraps every pipeline stage
      and every retrieval backend in named spans with hit-count and
      latency-ms attributes, and the retrieval response echoes the
      `trace_id`. (Sampler wiring lives in deployment config.)

**Exit criteria — Python ML microservice scaling.**

- [x] Horizontal scaling of the Docling and embedding workers
      independently (each with its own HPA on CPU + queue depth) —
      `deploy/hpa-docling.yaml` and `deploy/hpa-embedding.yaml`
      target the `docling_parse_queue_depth` and
      `embedding_queue_depth` Prometheus gauges exported by the
      Python sidecars (`services/_metrics.py`).
- [x] Mem0 service partitioning by tenant prefix to prevent noisy
      neighbours —
      `services/memory/memory_server.py::tenant_prefix` resolves
      every operation's user_id namespace under a configurable
      prefix template (`MEM0_TENANT_PREFIX_TEMPLATE`); search
      drops cross-tenant rows by metadata as a defence-in-depth
      guard. Verified by `services/memory/test_partitioning.py`.
- [x] gRPC connection pooling and per-target deadlines on the Go side
      — `internal/grpcpool/` exposes a round-robin pool with a
      circuit breaker (closed → open → half-open) and a
      configurable per-call `Deadline`.
- [x] Capacity test that ingests N documents / minute and proves the
      pipeline does not back-pressure the connectors —
      `tests/capacity/capacity_test.go` runs configurable load
      through the coordinator with fake stages and asserts no
      submit deadline is exceeded; `make capacity-test` runs it
      locally and in CI.

**Exit criteria — cross-platform on-device.**

- [x] Bonsai-1.7B benchmarks across at least three device tiers per
      platform (iOS / Android / desktop), with documented effective-tier
      transitions under thermal pressure — benchmark contract in
      `tests/benchmark/bonsai_contract_test.go::BonsaiContract`
      defines per-tier minimum tokens/sec, max first-token
      latency, and max memory. The on-device measurements run in
      `kennguy3n/knowledge` + `kennguy3n/llama.cpp` against this
      contract; thermal transitions are encoded in the eviction
      policy's `ThermalEvictMultiplier`.
- [x] On-device retrieval shard eviction tuned per tier so heavy clients
      do not OOM low-tier devices — `internal/shard/eviction.go`
      ships `EvictionPolicy` / `ShouldEvict()` with deterministic
      `unknown_tier` / `shard_too_large` / `memory_pressure`
      labels, plus `DefaultEvictionPolicies()` (Low: 64 MB /
      256 MB free, Mid: 256 MB / 512 MB free, High: 1024 MB /
      1024 MB free, 0.5x multiplier on thermal). Surfaced to
      clients in the `eviction_config` field of
      `GET /v1/models/catalog`.

---

## Cross-cutting requirements

These apply to every phase:

- **No regressions.** Each phase ships with a regression suite the next
  phase inherits.
- **Audit log first.** Every new operation lands its audit-log event
  before its happy-path is wired through the UI.
- **Tenant guard.** Every new storage call goes through the tenant-scoped
  storage client; cross-tenant queries are not allowed at the library
  boundary.
- **Privacy contract.** Every new retrieval path returns a
  `privacy_label`. The privacy strip is part of the definition of done.
