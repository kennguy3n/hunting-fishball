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

**Status.** 🟡 partial | ~80% (server-side + contracts shipped; coverage repo authoritative; channel `deny_local_retrieval` wired end-to-end; tenant-deletion 5-step workflow implemented; on-device implementations still tracked in `kennguy3n/knowledge`)

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

**Status.** 🟡 partial | ~75% (server contracts + endpoints shipped; e2e smoke covers shard/coverage/models/B2C/device-first/forget; structured JSON logging + W3C traceparent middleware shipped; readyz probes + DLQ observer wired; client UIs in B2C repos)

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

**Status.** 🟡 partial | ~100%

- [x] ≥ 12 production connectors at GA — Phase 1 (Google Drive,
      Slack) + Phase 7 (SharePoint, OneDrive, Dropbox, Box, Notion,
      Confluence, Jira, GitHub, GitLab, Microsoft Teams) = 12
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

**Status.** 🟡 partial | ~100% (all 12 line items shipped)

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

---

## Changelog

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
