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

**Status.** 🟡 partial | ~100%

- [x] `SourceConnector` interface defined in the platform backend
- [x] Optional `DeltaSyncer` / `WebhookReceiver` / `Provisioner` interfaces
- [x] Process-global connector registry (`Register*` / `Get*`)
- [x] AES-GCM credential encryption reused from the platform backend
- [x] Audit-log Postgres table + Kafka outbox
- [x] Audit-log API surfaced to the admin portal

## Phase 1 — Single-source MVP end-to-end

**Status.** 🟡 partial | ~100%

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

**Status.** 🟡 partial | ~100%

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

**Status.** 🟡 partial | ~100%

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
- [ ] Retrieval P95 < 500 ms on the sample corpus
      (in-process P95 ~178 µs measured in `tests/benchmark/`; full
      end-to-end latency target deferred to Phase 8 load tests)

## Phase 4 — Policy framework + simulator + privacy strip

**Status.** 🟡 partial | ~98%

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

**Status.** 🟡 partial | ~40% (server-side shipped; on-device pending)

- [ ] UniFFI XCFramework for iOS
- [ ] UniFFI AAR for Android
- [ ] N-API binding for desktop
- [x] On-device retrieval shard sync — server-side
      (`internal/shard/handler.go` mounts
      `GET /v1/shards/:tenant_id` and
      `GET /v1/shards/:tenant_id/delta?since=<v>`;
      `internal/shard/repository.go` is the GORM-backed metadata
      store; `migrations/006_shards.sql` defines `shards`)
- [x] Shard generation worker — policy-aware
      (`internal/shard/generator.go` calls `PolicyResolver.Resolve`
      to gate eligible chunks; wired into `cmd/ingest/main.go` as
      an optional post-Stage-4 hook for `shard.requested`)
- [x] Shard delta sync protocol — version-keyed add / remove
      (`internal/shard/delta.go`)
- [ ] Local-first retrieval, policy-bounded fallback (client side)
- [ ] Bonsai-1.7B GGUF via `llama.cpp` on at least one desktop + one mobile
- [x] Cryptographic forgetting on the on-device tier — server-side
      (`internal/shard/forget.go` orchestrates pending_deletion →
      drain → drop Qdrant / FalkorDB / Tantivy / Redis →
      destroy DEKs → mark deleted; `cmd/api/main.go` mounts
      `DELETE /v1/tenants/:tenant_id/keys`)

## Phase 6 — B2C client surfaces

**Status.** ⏳ planned

- [ ] iOS / Android / desktop B2C apps consume the same retrieval API
- [ ] On-device first by default
- [ ] Privacy strip in B2C clients
- [ ] Background sync per platform native scheduler

## Phase 7 — Catalog expansion

**Status.** 🟡 partial | ~85% (12 of 12 target connectors implemented;
runbooks + per-connector e2e smoke tests pending)

- [x] ≥ 12 production connectors at GA — Phase 1 (Google Drive,
      Slack) + Phase 7 (SharePoint, OneDrive, Dropbox, Box, Notion,
      Confluence, Jira, GitHub, GitLab, Microsoft Teams) = 12
- [ ] Per-connector runbooks
- [x] Per-connector capability matrix in this doc (see below)
- [ ] End-to-end smoke test green per connector

## Phase 8 — Cross-platform optimization

**Status.** 🟡 partial | ~50% (6 of 12 line items shipped; HPAs and
Python-side autoscaling tracked in the deployment-config repo)

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
- [ ] HPA on Kafka lag (ingest) and QPS (api)
- [x] OpenTelemetry trace sampling tuned for cost / tail-latency
      tradeoff — `internal/observability/tracing.go` centralises
      tracer + attribute keys; spans emitted around the four
      pipeline stages and the four retrieval backends
      (vector / bm25 / graph / memory) with hit-count + latency_ms
      attributes; `RetrieveResponse.TraceID` echoes the trace_id
      to the client per the API contract

Python ML microservice scaling:

- [ ] HPA on Docling worker (CPU + queue depth)
- [ ] HPA on embedding worker (CPU + queue depth)
- [ ] Mem0 partitioning by tenant prefix
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

- [ ] Bonsai-1.7B benchmarks across ≥ 3 tiers per platform
- [ ] On-device retrieval shard eviction tuned per tier

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

---

## Changelog

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
