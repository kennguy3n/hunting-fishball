# hunting-fishball

> **Status (2026-05-12, post-Round-14).** Phases 0-3 and 7-8 are
> **functionally complete** in `main`; Phases 4-6 are
> **server-side complete** with only client-side rendering left in
> external repos. See [`docs/PROGRESS.md`](docs/PROGRESS.md) for
> the live checklist and the Round-by-round changelog below.
> Migration count is at **040**. Round 14 layers observability
> dashboards (stage breakers, latency histogram, slow-query
> persistence, pipeline throughput), payload-schema validation,
> the audit-integrity background worker, the API-key grace
> sweeper, per-tenant payload caps, embedding-fallback metrics,
> DLQ categorisation, four new Prometheus alerts, a regression
> manifest + e2e suite + four fuzz targets, OpenAPI completeness
> through the Round-13/14 surface, and a CI fast-lane split into
> `fast-check` / `fast-test` / `fast-build` (with per-job
> `actions/cache` on `~/.cache/go-build`). See the Round-14
> additions block below for the per-task breakdown.
>
> Per-phase detail lives in [`docs/PROGRESS.md`](docs/PROGRESS.md)
> and [`docs/PHASES.md`](docs/PHASES.md); the product thesis is in
> [`docs/PROPOSAL.md`](docs/PROPOSAL.md) and the target system
> design in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

`hunting-fishball` is a privacy-preserving **knowledge & context platform** that
unifies an organization's documents, chat history, files, and SaaS records into
a single, governed knowledge plane — and exposes that plane to AI agents,
on-device SLMs, and end-user clients across **mobile, desktop, and web**.

The platform is a multi-tenant control plane sitting between:

- **Source systems** — SharePoint, Google Drive, OneDrive, Dropbox, Box, Notion,
  Confluence, Jira, GitHub, Slack, chat history, uploaded files, and 150+ SaaS
  connectors over time.
- **Knowledge consumers** — admin portal, mobile apps, desktop clients, AI
  agents, on-device Small Language Models (SLMs), and third-party integrations.

It is opinionated about three things:

1. **Local-first where possible.** Sensitive content is summarized, indexed,
   and retrieved on-device whenever the device tier supports it. The platform
   never forces remote processing for content the device can handle locally.
2. **Privacy-first by default.** Every retrieval call carries a privacy label
   that surfaces *what data was processed where* in the UI, and the policy
   simulator lets admins evaluate the data-flow impact of any rule change
   before it ships.
3. **Polyglot by necessity, not by accident.** The control plane, context
   engine, and retrieval API are **Go**, matching the existing platform
   backend. Python is retained only as **thin ML microservices** (document
   parsing, embedding computation, GraphRAG entity extraction, persistent
   memory) accessed via gRPC.

---

## Why this exists

Every product surface that wants to "answer over the customer's stuff" — an
inbox digest, a chat copilot, a desktop search bar, a mobile assistant — ends
up rebuilding the same five things:

1. Connectors that ingest from SaaS sources and chat history.
2. A pipeline that fetches, parses, embeds, and stores.
3. A retrieval layer that fans out across vector / lexical / graph / memory.
4. An evaluation harness that knows whether retrieval is actually any good.
5. A privacy + policy framework that tells admins what data is going where.

`hunting-fishball` builds those once, multi-tenant, multi-platform, and exposes
them as **connector-agnostic, client-agnostic** APIs. Skill packs, chat
assistants, and agent runtimes live *on top of it* (see `chat-b2b-skills`,
`chat-b2c-skills`, `slm-rich-media`, `cv-guard`). They do not reinvent
ingestion, retrieval, or governance.

---

## Tech Stack

| Layer | Technology |
|---|---|
| Admin Portal | React (fe-admin) |
| Mobile | iOS (Swift / SwiftUI + UniFFI), Android (Kotlin / Compose + UniFFI) |
| Desktop | Electron + React + N-API bindings |
| Platform Backend | Go (Gin, GORM, PostgreSQL) |
| Context Engine | Go (Kafka consumer, retrieval API, caching, pipeline orchestration) |
| ML Microservices | Python (Docling document parsing, embedding computation, GraphRAG construction) |
| Knowledge Core | Rust (from `kennguy3n/knowledge`) |
| On-device SLM | Bonsai-1.7B via `llama.cpp` |

The platform backend (`ai-agent-platform`) is already Go on Gin, GORM,
PostgreSQL, Redis, and Kafka. The Go context engine reuses that toolchain
end-to-end (deployment model, auth/tenancy patterns, observability stack,
operational runbooks). Python lives only where the ML ecosystem libraries
require it, behind gRPC interfaces — so Python services can be replaced with
Go implementations as Go ML tooling matures, without re-architecting the
control plane.

---

## Architecture overview

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Clients                                                                │
│    Admin Portal (React)                                                 │
│    Mobile (iOS Swift/SwiftUI + UniFFI / Android Kotlin/Compose + UniFFI)│
│    Desktop (Electron + React + N-API)                                   │
└──────────────────────────┬──────────────────────────────────────────────┘
                           │  REST / WebSocket / SSE
                           ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Platform Backend (Go — Gin, GORM, PostgreSQL)                          │
│    Tenancy, IAM, billing, connector registry, source management,        │
│    knowledge nodes, audit log, control-plane APIs                       │
└──────────────────────┬──────────────────────────┬───────────────────────┘
                       │  Kafka                   │  HTTP (Gin)
                       ▼                          ▼
┌──────────────────────────────────┐   ┌──────────────────────────────────┐
│  Context Engine (Go)             │   │  Retrieval API (Go — Gin)        │
│    Kafka consumer (sarama /      │   │    Vector  (Qdrant Go client)    │
│      confluent-kafka-go)         │   │    BM25    (tantivy-go)          │
│    Pipeline coordinator          │   │    Graph   (FalkorDB Go client)  │
│      (goroutines + errgroup)     │   │    Memory  (gRPC → Mem0 svc)     │
│    Stage 1 Fetch (HTTP / S3)     │◀──│    Result merger + reranker      │
│    Stage 4 Storage (Qdrant /     │   │    Semantic cache (Redis)        │
│      FalkorDB / PostgreSQL)      │   └──────────────────────────────────┘
└──────────────────────────────────┘
                       │  gRPC
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Python ML Microservices (stateless workers, gRPC)                      │
│    Docling parsing service     — PDF / DOCX / XLSX / PPTX / HTML        │
│    Embedding service           — local model inference, batching        │
│    GraphRAG entity extractor   — entity / relation extraction           │
│    Mem0 memory service         — persistent user memory                 │
└─────────────────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Storage plane                                                          │
│    PostgreSQL (metadata)  Qdrant (vectors)  FalkorDB (graph)            │
│    Tantivy (BM25)         Redis (cache)     Object storage (artifacts)  │
└─────────────────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  On-device                                                              │
│    Knowledge Core (Rust, UniFFI / N-API)                                │
│    Bonsai-1.7B SLM via llama.cpp (CPU / Metal / CUDA / Vulkan / NPU)    │
│    Local caches, redaction, on-device retrieval shards                  │
└─────────────────────────────────────────────────────────────────────────┘
```

### Sync architecture (high level)

Source systems push change events into Kafka. The Go context engine consumes
those events with a goroutine-per-partition pool, runs a 4-stage pipeline, and
writes the resulting chunks / embeddings / graph nodes into the storage plane.
Stages 1 and 4 are pure Go; stages 2 (parse) and 3 (embed) call the Python ML
microservices via gRPC, or — for embedding — call a remote embedding API
directly when the tenant has opted into a hosted model.

Goroutines and channels replace the previous Python `multiprocessing`
`ProcessCoordinator`, giving the engine lightweight per-document concurrency
without the GIL. `errgroup` is used for structured concurrency inside the
retrieval API's parallel fan-out (vector / lexical / graph / memory).

### Retrieval architecture (high level)

`POST /v1/retrieve` accepts a query plus a tenant / user / channel scope and
fans out **in parallel**:

- **Qdrant** vector search (Go client) — semantic recall.
- **Tantivy** BM25 / hybrid search (`tantivy-go` bindings) — exact / lexical
  recall.
- **FalkorDB** graph traversal (Go client) — entity-anchored multi-hop.
- **Mem0** memory search (gRPC → Python) — user / session memory.

A Go merger + reranker combines the four streams, applies tenant-level policy
(privacy mode, allow / deny lists, per-channel ACLs), and returns a single
ranked list with provenance. Hot queries are cached in **Redis** via a
semantic-cache key (query embedding + scope hash).

For the long-form rationale on language choice and retrieval-layer mapping,
see [`docs/PROPOSAL.md` §3.1](docs/PROPOSAL.md#31-context-engine-language-choice).

The full set of public + admin endpoints is documented in
[`docs/openapi.yaml`](docs/openapi.yaml). Highlights added in the
2026-05-10 production-hardening batch:

- `POST /v1/retrieve/batch` — fan out up to 32 retrieves at once
  with a configurable `max_parallel` cap.
- `GET /v1/admin/audit` — search/filter audit logs by `action`,
  `resource_id`, `source_id`, free-text `q=` query, and time range.
- `GET /v1/admin/sources/:id/progress` — per-namespace sync
  progress (discovered / processed / failed / percent_done).
- `GET /v1/admin/dashboard` — connector + pipeline + retrieval
  health summary suited to an admin dashboard widget.
- `GET /v1/admin/dlq`, `GET /v1/admin/dlq/:id`,
  `POST /v1/admin/dlq/:id/replay` — dead-letter inspection +
  replay (`max_retries` guard).
- `POST /v1/admin/reindex` — re-emit Stage 2–4 events for an
  existing tenant / source / namespace without re-fetching from
  the upstream source.

**Round 5 (PR #14) additions:**

- `POST /v1/admin/sources/preview` — connector dry-run preview
  (validate credentials, enumerate namespaces, count docs).
- `POST /v1/admin/dlq/replay` — DLQ batch replay with filters
  (source_id, time range, error_contains).
- `POST /v1/retrieve` `explain: true` — retrieval explain/debug
  mode (vector similarity, BM25, graph depth, RRF, reranker).
- `POST /v1/retrieve/feedback` — retrieval feedback collection.
- `GET /v1/admin/policy/history` — policy version history
  (paginated).
- `POST /v1/admin/policy/rollback/:version_id` — rollback to
  a historical policy snapshot.
- `POST /v1/admin/tenants/:tenant_id/export` — GDPR data export.
- `GET /v1/admin/chunks/:chunk_id` — chunk provenance trace.
- `GET /v1/admin/analytics/global` — cross-tenant super-admin
  analytics (requires super_admin role).
- `make eval` — run eval quality gate against golden corpus.

**Round 6 additions:**

- `POST /v1/retrieve` `diversity: { lambda: 0.7 }` — MMR
  diversifier on the merged result set.
- `POST /v1/retrieve/stream` — Server-Sent-Events streaming
  retrieval (clients render partial results as each backend
  completes).
- `GET /v1/admin/sources/:id/schema` — connector schema discovery.
- `GET/PUT /v1/admin/sources/:id/embedding` — per-source embedding
  model config.
- `POST /v1/admin/synonyms` — manage tenant-scoped query
  expansion synonym sets.
- `POST/GET/DELETE /v1/admin/retrieval/experiments` — retrieval
  A/B testing experiment CRUD.
- `GET/POST/DELETE /v1/admin/notifications` — admin notification
  preferences (webhook / email).
- `GET/POST /v1/admin/connector-templates` — connector default
  config templates per tenant.
- `POST /v1/admin/isolation-check` — cross-tenant isolation
  audit report.
- `POST /v1/admin/tenants/:tenant_id/export` — full tenant data
  export (asynchronous job).
- `POST /v1/admin/reindex` `dry_run: true` — pipeline dry-run.
- `GET /v1/admin/pipeline/retry-stats` — pipeline stage retry
  analytics snapshot.
- Pipeline backpressure gauge
  `context_engine_pipeline_channel_depth{stage}` + alert rule in
  `deploy/alerts/pipeline_backpressure.yaml`.
- API versioning middleware emits `X-API-Version` on every
  response and rejects unsupported versions with 406.

**Round 7 additions:**

- `GET /v1/admin/analytics/queries` — retrieval query analytics
  with time-range, tenant, and top-N filters.
- `GET /v1/admin/notifications/delivery-log` — notification
  delivery attempts log (retry status, response code,
  `next_retry_at`).
- `GET /v1/admin/retrieval/experiments/:name/results` — A/B test
  results aggregator (per-arm latency, hit count, cache hit rate).
- `GET /v1/admin/sources/:id/credential-health` — connector
  credential validity (set by the periodic credential health
  worker).
- `POST /v1/admin/retrieval/warm-cache` — pre-warm the semantic
  cache for a list of `(tenant, query)` tuples; supports
  `auto_top_n` mode reading from `query_analytics`.
- `POST /v1/admin/sources/bulk` — bulk pause / resume /
  disconnect with per-source error isolation.
- `GET/PUT /v1/admin/tenants/:id/latency-budget` — per-tenant
  retrieval P95 budget.
- `GET /v1/admin/chunks/quality-report` — per-source chunk
  quality distribution (length, language, embedding magnitude,
  duplicate ratio).
- `GET /v1/admin/audit/export?format=csv|jsonl` — streamed audit
  trail export (chunked transfer encoding).
- `GET/PUT /v1/admin/tenants/:id/cache-config` — per-tenant
  semantic cache TTL.
- `GET /v1/admin/sources/:id/sync-history` — historical sync
  runs with status / duration / docs processed / docs failed.
- `POST/GET/DELETE /v1/admin/retrieval/pins` — pinned retrieval
  results.
- `GET /v1/admin/pipeline/health` — per-stage pipeline health
  dashboard (throughput, P50/P95 latency, retry, queue depth,
  DLQ totals).
- `migrations/rollback/015..031_*.down.sql` — full per-migration
  rollback coverage; `make migrate-rollback` applies them in
  reverse order.

**Round 13 additions:**

- New admin endpoints:
  - `GET /v1/admin/health/summary` — Round-13 Task 1.
    Parallel fan-out to every health probe (Postgres, Redis,
    Qdrant, Kafka, gRPC sidecars, credential health) returning
    a verdict (`healthy` | `degraded` | `unhealthy`) plus
    per-component latency / error
    (`internal/admin/health_summary_handler.go`).
  - `GET /v1/admin/analytics/queries/slow` — Round-13 Task 8.
    Lists recent retrievals exceeding
    `CONTEXT_ENGINE_SLOW_QUERY_THRESHOLD_MS` (default 1000 ms)
    with per-backend timings (`internal/admin/slow_query.go`).
  - `GET /v1/admin/analytics/cache-stats` — Round-13 Task 9.
    Per-tenant cache hit / miss / hit_rate_pct over a
    configurable window
    (`internal/admin/cache_stats_handler.go`).
  - `POST /v1/admin/tenants/:tenant_id/rotate-api-key` —
    Round-13 Task 10. Returns the new key exactly once; old key
    valid for `CONTEXT_ENGINE_API_KEY_GRACE_PERIOD` (default
    24 h) (`internal/admin/api_key_rotation.go`).
  - `GET /v1/admin/audit/integrity` — Round-13 Task 12.
    SHA-256 hash-chain head over audit rows (deterministic via
    oldest-first sort), so operators can detect tampered or
    deleted rows (`internal/audit/integrity.go`).
- Pipeline + reliability hardening: per-stage circuit breakers
  for Parse / Embed (Task 5, `internal/pipeline/stage_breaker.go`),
  DLQ age monitor + `DLQAgeHigh` alert (Task 4), payload size
  limiter middleware rejecting bodies >
  `CONTEXT_ENGINE_MAX_REQUEST_BODY_BYTES` (default 10 MiB)
  with HTTP 413 (Task 11), Postgres pool leak detector that
  warns on sustained > 90 % utilisation (Task 18), and a
  degraded-mode Go-native embedding fallback that activates
  when the gRPC sidecar circuit breaker opens (Task 19,
  `internal/pipeline/embed_fallback.go`).
- Observability: SLO multi-window burn-rate alerts in
  `deploy/alerts/slo_burn_rate.yaml` (Task 2), parent-trace
  correlation across `POST /v1/retrieve/batch` (Task 3),
  per-backend contribution breakdown in
  `internal/retrieval/explain.go` (Task 7), aggregated
  `percent_complete` on the sync-progress endpoint (Task 6).
- Testing depth: chaos-Kafka e2e
  (`tests/e2e/chaos_kafka_test.go`, Task 13), concurrent
  delete race e2e (`tests/e2e/concurrent_delete_test.go`,
  Task 14), eval corpus expanded from 20 → 50 cases covering
  multi-hop graph, BM25 exact-match, memory-augmented, and
  cross-namespace queries (`tests/eval/golden_corpus.json`,
  Task 15).
- Developer experience: `make doctor` checks all prerequisites
  (Task 16, `scripts/doctor.sh`); `docs/openapi_test.go`'s new
  `TestOpenAPI_RouterCoverage` walks the gin router and
  asserts every registered `/v1/` route has a path entry in
  `docs/openapi.yaml` (Task 17).
- CI / infrastructure: `fast-go` split with `fast-lint` in
  parallel (Task 0, brings fast-lane under 3 min); new
  `make migrate-dry-run-pg` + `full-migrate-dry-run-pg` CI job
  catches Postgres-specific syntax (JSONB / TIMESTAMPTZ /
  `ADD COLUMN IF NOT EXISTS`) that the SQLite dry-run misses
  (Task 20, `scripts/migrate-dry-run-pg.sh`).

**Round 14 additions:**

- New observability admin endpoints:
  - `GET /v1/admin/pipeline/breakers` — Round-14 Task 1.
    Per-stage circuit-breaker dashboard (state, fail_count,
    opened_at, probe_in_flight) backed by an in-process
    `StageBreakerRegistry`
    (`internal/admin/stage_breaker_handler.go`).
  - `GET /v1/admin/retrieval/latency-histogram` — Round-14
    Task 2. P50/P75/P90/P95/P99 per backend (vector / bm25 /
    graph / memory / merge / rerank / total) over a configurable
    window, computed from a 60-bucket in-process ring buffer
    (`internal/admin/latency_histogram_handler.go`).
  - `GET /v1/admin/retrieval/slow-queries` — Round-14 Task 3.
    Lists persisted slow queries from the new `slow_queries`
    table (`migrations/038_slow_queries.sql`,
    `internal/admin/slow_query_log_handler.go`).
  - `GET /v1/admin/pipeline/throughput?window=5m` — Round-14
    Task 4. Per-stage event counts + avg latency from a
    1-minute-bucket ring buffer
    (`internal/admin/pipeline_throughput_handler.go`).
- Security & resilience:
  - Two-layer request payload validator (JSON decode +
    struct-field check) for `/v1/retrieve`, `/v1/retrieve/batch`,
    and `/v1/retrieve/stream`; rejection emits
    `ERR_INVALID_PAYLOAD` (Task 5).
  - Audit log integrity background worker gated on
    `CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK=true` and
    `CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK_INTERVAL`
    (default 1 h). Increments
    `context_engine_audit_integrity_violations_total` via a
    callback so the audit package has no observability
    dependency (Task 6).
  - API-key grace sweeper transitions rotated keys past
    `grace_until` to `expired`; new
    `context_engine_api_keys_expired_total` and
    `_grace_expiring_soon` metrics (Task 7).
  - Per-tenant payload size overrides via
    `migrations/039_tenant_payload_limits.sql` and the new
    `TenantPayloadLookup` adapter pluggable into the existing
    payload limiter (Task 8).
- Testing depth: regression manifest
  `tests/regression/round1213_manifest.go` (Task 9), Round-13
  e2e suite under `e2e` build tag in
  `tests/e2e/round13_test.go` (Task 10), four new fuzz targets
  for API-key JSON, stage breaker concurrency, health-summary
  JSON, and slow-query threshold parsing wired into
  `make fuzz` (Task 11), and a re-audited cache invalidation
  baseline through Round 13 (Task 12).
- Pipeline hardening: embedding-fallback path now emits
  `context_engine_embedding_fallback_total{reason}` +
  `_embedding_fallback_latency_seconds` (Task 13). DLQ rows
  carry a `category` column (`transient` | `permanent` |
  `unknown`) via `migrations/040_dlq_category.sql`; the auto
  replayer skips permanent rows and the admin list endpoint
  exposes a `?category=` filter (Task 14).
- CI: `.github/workflows/ci.yml` splits `fast-go` into three
  parallel sub-jobs (`fast-check`, `fast-test`, `fast-build`)
  and pins `actions/cache` on `~/.cache/go-build` per job
  (Tasks 15-16). `full-e2e` now provisions Docker Buildx so
  pulled image layers persist across runs.
- OpenAPI: typed schemas for the Round-13 admin surface plus
  the new Round-14 endpoints, pinned via
  `docs/openapi_test.go` (Task 17).
- Alerts: four new entries in `deploy/alerts.yaml` —
  `AuditIntegrityViolation` (page), `EmbeddingFallbackRateHigh`,
  `APIKeyGraceExpiringSoon`, and `SlowQueryRateHigh` (all
  warning). Validated by `make alerts-check` (Task 18).

**Round 12 additions:**

- Three new Prometheus alerts in `deploy/alerts.yaml`:
  `GRPCCircuitBreakerOpen` (page, fires when
  `context_engine_grpc_circuit_breaker_state{target} == 2` for
  >5m), `PostgresPoolSaturated`, and `RedisPoolSaturated` (both
  fire when their pool's open connections exceed 80% of the
  configured maximum for >5m). All three validated by
  `make alerts-check`.
- Retention worker emits
  `context_engine_retention_expired_chunks_total` (counter) and
  `context_engine_retention_sweep_duration_seconds` (histogram)
  per sweep; metrics registered in
  `internal/observability/metrics.go`.
- The scheduler tick is now wrapped in `SafeTick` —
  `recover()`-guarded so a panic in any emitter cannot kill the
  goroutine. Recovered panics + propagated errors bump
  `context_engine_scheduler_errors_total`; `last_error` /
  `last_error_at` persist on the `sync_schedules` row.
- Python sidecars (`services/docling`, `services/embedding`,
  `services/memory`) register the gRPC health protocol
  (`grpc_health.v1`). Each ships a pytest covering
  `Check() == SERVING`.
- New background worker:
  `internal/pipeline/dlq_auto_replay.go` periodically scans
  `dlq_messages` for rows with `replay_count < max_auto_retries`
  and re-emits them with capped exponential backoff (1m / 5m /
  30m). Gated on `CONTEXT_ENGINE_DLQ_AUTO_REPLAY=true`. Metric:
  `context_engine_dlq_auto_replays_total`.
- New endpoint:
  `GET /v1/admin/sources/:id/rate-limit-status` returns the
  adaptive rate limiter's current token state — `current_tokens`,
  `max_tokens`, `effective_rate`, `halve_count`, `last_429_at`,
  `is_throttled`. Built on the `RateLimitInspector` interface so
  tests stay hermetic.
- Audit log retention: a background sweeper deletes
  `audit_logs` rows older than
  `CONTEXT_ENGINE_AUDIT_RETENTION_DAYS` (default 90) in batches
  of 1000 rows. Metric:
  `context_engine_audit_rows_expired_total`. Migration
  `034_audit_retention.sql` adds the `created_at` index.
- Adaptive rate limiter now exports
  `context_engine_adaptive_rate_current{connector}` (gauge) and
  `context_engine_adaptive_rate_halved_total{connector}`
  (counter).
- Four new Go-native fuzz targets cover
  `pipeline.ParsePartitionKey`, `policy.EffectiveMode`,
  `shard.DeltaDiff`, and `admin.QueryHash`; each fuzz target
  appears as its own line in the `make fuzz` enumeration.
- New CI gates: `make eval` (fails if Precision@5 < 0.8 on
  the golden corpus) and `make migrate-dry-run` (runs the
  migrate runner with `DryRun=true` against a fresh SQLite
  database). Both run in the fast lane.
- New structural tests:
  `internal/retrieval/cache_invalidation_test.go` (AST-based —
  every `QdrantClient.Upsert` / `FalkorDBClient.WriteNodes` /
  `BleveClient.Index` call must be paired with a
  `cache.Invalidate` call) and
  `tests/integration/rbac_coverage_test.go` (every route under
  `/v1/admin/` must have the RBAC middleware in its handler
  chain).
- `docs/openapi.yaml` adds 34 typed response schemas
  (FeedbackResponse, EvalRunReport, SyncSchedule, ModelCatalog,
  CredentialHealth, ExportJob, Experiment, WarmCacheResponse,
  BulkSourceResponse, ChunkQualityReport, LatencyBudget,
  CacheConfig, SyncHistoryList, PinnedResultList,
  ConnectorTemplateList, Synonyms, NotificationPreferenceList,
  NotificationDeliveryLog, RateLimitStatus, …) and typed
  request bodies for `/v1/retrieve/batch`, `/v1/retrieve/stream`,
  `/v1/admin/sources`, `/v1/admin/policy/simulate`.
- New tests: `tests/e2e/isolation_smoke_test.go` (Round-12 Task
  7), `tests/e2e/round12_test.go` (Round-12 surface bundle),
  and `tests/regression/round911_manifest.go` (PR #20
  Devin Review fixes catalogue).

**Round 11 additions:**

- The `Makefile` `fuzz` target now enumerates each fuzz target
  individually (one `go test -fuzz='^FuzzName$'` invocation
  per target) so Go's "pattern must match exactly one target"
  rule cannot fire on the nightly job.
- The pipeline's Stage-4 hot-path GORM hooks
  (`scoreAndRecordBlocks`, `recordSyncStart`,
  `FinishBackfillRun`) are wrapped in `runWithHookTimeout`,
  configurable via `CONTEXT_ENGINE_HOOK_TIMEOUT` (default
  500ms). A new `HookTimeoutsTotal{hook}` counter tracks
  expirations. The chunk-quality hook additionally increments
  `context_engine_chunk_quality_errors_total` and emits a
  structured `slog.Warn` on recorder failures.
- The batch retrieval handler now threads the request's
  `Diversity` (MMR lambda) into every sub-request — previously
  the field was silently dropped on the batch path. The SSE
  streaming handler now emits the per-backend explain trace
  inside every event when `explain: true`.
- The shard pre-generator consults the policy snapshot's
  chunk-ACL after the policy gate, filtering out chunks the
  shard's audience cannot read.
- `query_analytics` adds a `source` column
  (`user` | `cache_warm` | `batch`) so operators can
  distinguish organic traffic from warm-up and batch calls.
  Migration `033_query_analytics_source.sql` (+ rollback)
  ships with the change.
- `/readyz` now returns per-backend latency in a
  `latencies` map (`postgres_ms`, `redis_ms`, `qdrant_ms`),
  distinguishing "up-but-slow" from "up-and-healthy".
- Four new Prometheus alerts:
  `ChunkQualityScoreDropped`, `CacheHitRateLow`,
  `CredentialHealthDegraded`, `GORMStoreLatencyHigh`.
  Validate with `make alerts-check`.
- Prometheus cardinality policy is now enforced by test: no
  metric may use `tenant_id` as a label. Tenant identity goes
  into `slog.With("tenant_id", ...)` log fields only.
- 10 new structured admin error codes in
  `internal/errors/catalog.go` (replaces ad-hoc
  `gin.H{"error": ...}` returns).
- `migrations/migration_order_test.go` enforces no duplicate
  prefixes, monotonic numbering from `001`, and rollback
  parity. Replaces the weaker rollback file check.
- `internal/retrieval/graceful_degradation.go` wraps every
  GORM lookup on the retrieve hot path (latency budget,
  cache TTL, pin list) in a 200ms timeout + panic recovery
  envelope. Failures bump
  `context_engine_gorm_store_lookup_errors_total{store}` and
  the handler degrades to defaults rather than 500.
- `docs/openapi.yaml` replaces `additionalProperties: true`
  stubs with typed schemas for the most-used endpoints
  (`/v1/health`, `/v1/admin/audit`, `/v1/admin/sources`,
  `/v1/admin/dlq`, `/v1/admin/policy/drafts`,
  `/v1/admin/pipeline/health`, `Capabilities`,
  `DashboardResponse`).
- New tests: `tests/e2e/round10_test.go` (Round-10 hook e2e),
  `tests/e2e/round11_test.go` (Round-11 e2e), and
  `tests/regression/round910_manifest.go` (PR #18/#19
  Devin Review fixes catalogue).

**Round 10 additions:**

- The retrieval handler now reads the tenant's
  `max_latency_ms` from the GORM `LatencyBudgetStore` via the new
  `LatencyBudgetLookup` port whenever the request omits
  `limits.max_latency_ms`. The handler enforces the resolved
  budget as a `context.WithTimeout` deadline.
- The Redis semantic cache (`internal/storage/redis_cache.go`)
  now reads per-tenant TTL through the GORM `CacheConfigStore`
  via the new `CacheTTLLookup` port. `Set` falls back to the
  global default only when no row exists.
- The pipeline coordinator records sync history on every
  backfill kickoff (`SyncHistoryRecorder` port → GORM
  `SyncHistoryStore`). Backfills produce rows in
  `sync_history` with start/end/status/docs_processed/docs_failed.
- The coordinator runs `pipeline.ChunkScorer` as a Stage-4
  pre-write hook (gated by
  `CONTEXT_ENGINE_CHUNK_SCORING_ENABLED`); scored chunks persist
  through `internal/admin/chunk_quality_gorm.go`.
- Periodic credential-health (`CONTEXT_ENGINE_CREDENTIAL_CHECK_INTERVAL`)
  and token-refresh (`CONTEXT_ENGINE_TOKEN_REFRESH_INTERVAL`) workers
  run in `cmd/api/main.go`; periodic retention
  (`CONTEXT_ENGINE_RETENTION_ENABLED`) and cron-scheduler
  (`CONTEXT_ENGINE_SCHEDULER_ENABLED`) workers run in
  `cmd/ingest/main.go`. All four register on the lifecycle
  shutdown runner.
- Retrieval handler grew `SetQueryExpander`, wiring the
  production `SynonymExpander` (backed by the GORM
  `SynonymStoreGORM`) into the request hot path. The full chain
  is pinned by `tests/integration/query_expansion_test.go`.
- New e2e tests: `tests/e2e/round9_test.go` pins the five
  Round-9 GORM admin surfaces with tenant isolation;
  `tests/e2e/round9_pipeline_test.go` pins the per-stage parse
  timeout and the cache-warm-on-miss behaviours.
- `internal/admin/handler_fuzz_test.go` ships fuzz targets for
  `ABTestConfig`, `ConnectorTemplate`, and
  `NotificationPreference` JSON unmarshalling. `make fuzz`
  now covers `./internal/admin/...` in addition to
  `./internal/retrieval/...`.
- `docs/openapi_test.go` `requiredPaths` now covers every
  route registered in `cmd/api/main.go`; missing entries were
  added to `docs/openapi.yaml`.
- `docs/runbooks/runbook_test.go` pins every registered
  connector against a matching `docs/runbooks/<name>.md` and
  asserts the four required sections (credential rotation,
  quota / rate-limit, outage detection, error codes).
- CI gains a `fast-proto-check` job (`make proto-check`) in the
  fast lane, `full-connector-smoke` / `full-bench-e2e` /
  `full-capacity-test` jobs in the full lane, and a nightly
  `nightly-fuzz` job that runs `make fuzz`.

**Round 9 additions:**

- Final five admin GORM stores landed — `NotificationStore`,
  `ABTestStore`, `ConnectorTemplateStore`, `SynonymStore`, and
  `ChunkQualityStore` are now Postgres-backed via GORM. After
  Round 9 there are no in-memory store fallbacks in
  `cmd/api/main.go`.
- New env vars for pipeline & retrieval tuning:
  `CONTEXT_ENGINE_FETCH_TIMEOUT` / `_PARSE_TIMEOUT` /
  `_EMBED_TIMEOUT` / `_STORE_TIMEOUT` give each pipeline stage an
  independent deadline that's reset per retry attempt;
  `CONTEXT_ENGINE_CACHE_WARM_ON_MISS=true` decouples cache writes
  from response latency by issuing them on a fire-and-forget
  goroutine.
- The RRF merger collapses duplicate `chunk_id`s from multiple
  retrieval backends before reranking (keeping the highest score).
- `POST /v1/retrieve/batch` now honours `explain:true` and threads
  the per-result explain trace through every batch entry.
- The gRPC sidecar pool publishes
  `context_engine_grpc_circuit_breaker_state{target}` to
  Prometheus (`0=closed`, `1=half-open`, `2=open`).
- Three new connection-pool gauges
  (`context_engine_postgres_pool_open_connections`,
  `_redis_pool_active_connections`,
  `_qdrant_pool_idle_connections`) plus a 30-second sampler
  goroutine in `cmd/api`.
- `deploy/recording-rules.yaml` ships pre-computed Prometheus
  series — retrieval availability, P95 latency, pipeline
  throughput, error rate, cache hit rate. `make alerts-check`
  validates both alerts and recording rules.
- `services/graphrag/test_graphrag.py` — 13 unit tests covering
  the Python GraphRAG entity/edge extractor.
- 7 new error codes in `internal/errors/catalog.go`
  (`ERR_CACHE_WARM_FAILED`, `ERR_BUDGET_INVALID`,
  `ERR_BUDGET_LOOKUP_FAILED`, `ERR_CACHE_CONFIG_FAILED`,
  `ERR_SYNC_HISTORY_FAILED`, `ERR_PINNED_RESULTS_FAILED`,
  `ERR_PIPELINE_HEALTH_FAILED`) wired into the admin error
  envelope.
- New e2e tests: `tests/e2e/notification_lifecycle_test.go`
  drives the full dispatch → retry → dead-letter loop, and
  `tests/e2e/pipeline_priority_test.go` drives the steady vs.
  backfill priority buffer and the semantic dedup pass.
- `tests/regression/round78_manifest.go` catalogues the Devin
  Review fixes from PRs #16 and #17 with each one's regression
  test, pinned by a meta-test.
- `docs/openapi.yaml` extended with the previously-undocumented
  Round-8 admin endpoints (`/v1/admin/retrieval/pins`,
  `/v1/admin/analytics/queries`, `/v1/admin/health/indexes`,
  `/v1/admin/sources/{id}/sync/stream`,
  `/v1/admin/sources/{id}/rotate-credentials`,
  `/v1/webhooks/{connector}/{source_id}`, `/v1/admin/dlq/replay`);
  spec parity is now pinned by `docs/openapi_test.go`.

**Round 8 additions:**

- Stage 4 store worker now consults `pipeline.Deduplicator`
  before persistence when `CONTEXT_ENGINE_DEDUP_ENABLED=true`.
- Coordinator `Submit` routes through `pipeline.PriorityBuffer`
  when `CONTEXT_ENGINE_PRIORITY_ENABLED=true`.
- Stage 3 embed worker honours per-source embedding-model
  overrides from `source_embedding_config`.
- `pipeline.RetryAnalytics` is now wired into `runWithRetry`
  so `GET /v1/admin/pipeline/retry-stats` reflects real
  ingest activity.
- `NotificationDispatcher` is wired into the audit pipeline;
  webhook / email subscribers fire on matching audit events
  (e.g. `source.connected`, `policy.promoted`,
  `source.credential_invalid`).
- All six in-memory admin stores (query analytics, pinned
  results, sync history, latency budget, cache config,
  credential health) are now Postgres-backed via GORM and
  wired into `cmd/api/main.go`.
- Credential health is checked periodically by a background
  worker in `cmd/ingest/main.go`; interval is configurable
  via `CONTEXT_ENGINE_CREDENTIAL_HEALTH_INTERVAL`
  (default `1h`).
- The retrieval handler now applies operator-pinned chunks
  (`pin_apply.ApplyPins`) after policy filtering and consults
  per-tenant latency budgets and cache TTL overrides via
  `Handler.SetLatencyBudgetLookup` / `SetCacheTTLLookup`.
- Notification deliveries that fail with a retryable code are
  persisted with `next_retry_at`; a background retry worker
  re-delivers with linear backoff and dead-letters after 5
  attempts.
- New operational runbook at
  [`docs/runbooks/operational.md`](docs/runbooks/operational.md)
  covers warming the cache, A/B testing, pipeline health,
  audit export, latency / TTL management, bulk source ops,
  chunk quality, and credential health alerts.

---

## Quick start

```bash
# 1. Clone
git clone https://github.com/kennguy3n/hunting-fishball.git
cd hunting-fishball

# 2. Install Go dependencies (Go 1.25+ required)
go mod download

# 3. Bring up the local storage plane (Postgres / Redis / Kafka / Qdrant)
docker compose up -d

# 4. Run the test suite
make test           # = go test -race -cover ./...

# 5. Run the end-to-end smoke test against the docker-compose stack
make test-e2e       # = E2E_ENABLED=1 go test -tags=e2e ./tests/e2e/...

# 6. (Phase 3) Run the Go ↔ Python integration tests against the ML services
make test-integration

# 7. (Phase 3) Run the throughput / latency benchmarks
make bench

# 8. (Phase 8) Run the capacity harness against the in-process pipeline
make capacity-test  # tunable via CAPACITY_DOCS_PER_MIN / CAPACITY_DURATION

# 9. (Phase 7) Run the per-connector e2e smoke suite
make test-connector-smoke   # = E2E_ENABLED=1 go test -tags=e2e ./tests/e2e/connector_smoke_test.go

# 10. (Phase 1/3) Run the end-to-end P95 retrieval-latency budget enforcement
make bench-e2e      # fails if retrieval P95 > 500 ms or round-trip P95 > 1 s

# 11. Generate proto stubs (only needed when proto files change)
make proto-gen

# 12. Build the binaries
make build          # produces ./bin/context-engine-ingest and ./bin/context-engine-api
```

The API and ingest binaries each expose three operational endpoints:

- `GET /healthz` — liveness probe (always 200 if the process is alive).
- `GET /readyz`  — readiness probe (Postgres / Redis / Qdrant for the
  API; Postgres / Redis / Kafka brokers for ingest). Returns 503 if
  any required dependency is down.
- `GET /metrics` — Prometheus scrape endpoint.
  See [`internal/observability/metrics.go`](internal/observability/metrics.go)
  for the collector list.

The Python ML sidecars (docling, embedding) expose `/metrics` on a
separate sidecar HTTP listener (default port 9090, override with
`METRICS_PORT`).

Other targets:

| Target                  | What it does                                                |
|-------------------------|-------------------------------------------------------------|
| `make test`             | `go test -race -cover ./...`                                |
| `make test-e2e`         | Bring up docker compose, run e2e smoke test                 |
| `make test-integration` | Bring up Phase 3 ML services, run integration tests         |
| `make services-test`    | Run Python unit tests for `services/{docling,embedding,memory}` |
| `make services-protos`  | Regenerate Python gRPC stubs into `services/_proto/`        |
| `make bench`            | Run pipeline + retrieval benchmarks in `tests/benchmark/`   |
| `make capacity-test`    | Run Phase 8 capacity harness in `tests/capacity/` (configurable via `CAPACITY_DOCS_PER_MIN` / `CAPACITY_DURATION`) |
| `make test-connector-smoke` | Run the Phase 7 per-connector e2e smoke suite (`tests/e2e/connector_smoke_test.go`, build tag `e2e`) |
| `make bench-e2e`        | Enforce the Phase 1/3 retrieval P95 budget in `tests/benchmark/p95_e2e_test.go` (build tag `e2e`) |
| `make build`            | Build both binaries into `./bin/`                           |
| `make vet`              | `go vet ./...`                                              |
| `make fmt`              | `gofmt -s -w` over hand-written sources                     |
| `make lint`             | `golangci-lint run` (skipped if not installed)              |
| `make proto-gen`        | Regenerate `*.pb.go` from `proto/**/*.proto`                |
| `make proto-check`      | Verify generated proto files are up to date                 |
| `make alerts-check`     | Validate `deploy/alerts.yaml` Prometheus rule manifest      |
| `make fuzz`             | Run Go native fuzz targets across `internal/retrieval/`, `internal/admin/`, `internal/pipeline/`, `internal/policy/`, `internal/shard/` (30s per target) |
| `make migrate-rollback` | Apply `migrations/rollback/*.down.sql` in reverse via `psql` (gated on `CONTEXT_ENGINE_DATABASE_URL`) |
| `make migrate-dry-run`  | Run `internal/migrate/runner.go` with `DryRun=true` against a fresh SQLite database — catches SQL syntax errors before merge (Round 12 Task 9) |
| `make migrate-dry-run-pg` | Launch a disposable Postgres 16 container and replay every up + rollback migration in lexical order — catches PG-specific syntax (JSONB / TIMESTAMPTZ / `ADD COLUMN IF NOT EXISTS`) the SQLite dry-run misses (Round 13 Task 20) |
| `make doctor`           | Check developer prerequisites: Go ≥ 1.25, Docker, docker-compose, Python 3.11+, protoc, golangci-lint, e2e env vars (Round 13 Task 16) |
| `make test-isolation`   | Run the Round-12 tenant-isolation e2e smoke (`tests/e2e/isolation_smoke_test.go`, build tag `e2e`) |
| `make eval`             | Run the golden-corpus eval; fails if Precision@5 < 0.6 (Round 13 Task 15 — 50-case corpus, thresholds unchanged) |
| `make clean`            | Remove `./bin/` and coverage artefacts                      |

### Python ML microservices (Phase 3)

Three thin gRPC servers wrap the heavy Python ML libraries:

```bash
# Build & run all three behind their docker-compose service names.
docker compose up -d docling embedding memory
```

| Service     | Port  | Wraps                                                            |
| ----------- | ----- | ---------------------------------------------------------------- |
| `docling`   | 50051 | [Docling](https://github.com/DS4SD/docling) document parser      |
| `embedding` | 50052 | [sentence-transformers](https://www.sbert.net) embedding model    |
| `memory`    | 50053 | [Mem0](https://github.com/mem0ai/mem0) persistent memory          |

The Go retrieval handler picks each backend up via environment
variables (`CONTEXT_ENGINE_BM25_DIR`, `CONTEXT_ENGINE_REDIS_URL`,
`CONTEXT_ENGINE_FALKOR_ENABLED`, `CONTEXT_ENGINE_MEMORY_TARGET`).
With every flag unset it gracefully degrades to the Phase 1
vector-only behaviour. See [`docs/CUTOVER.md`](docs/CUTOVER.md) for
the staged rollout plan.

## Project structure

```
hunting-fishball/
├── cmd/
│   ├── ingest/                # context-engine-ingest binary entry point
│   └── api/                   # context-engine-api    binary entry point
├── internal/
│   ├── connector/             # SourceConnector interface, optional
│   │   │                      # interfaces, process-global registry.
│   │   │                      # 12 connectors at Phase 7:
│   │   ├── googledrive/       # Google Drive (Phase 1)
│   │   ├── slack/             # Slack + Events API (Phase 1)
│   │   ├── sharepoint/        # SharePoint Online (Phase 7)
│   │   ├── onedrive/          # OneDrive personal (Phase 7)
│   │   ├── dropbox/           # Dropbox v2 (Phase 7)
│   │   ├── box/               # Box (Phase 7)
│   │   ├── notion/            # Notion (Phase 7)
│   │   ├── confluence/        # Confluence Cloud (Phase 7)
│   │   ├── jira/              # Jira Cloud (Phase 7)
│   │   ├── github/            # GitHub (Phase 7)
│   │   ├── gitlab/            # GitLab (Phase 7)
│   │   └── teams/             # Microsoft Teams (Phase 7)
│   ├── credential/            # AES-256-GCM envelope encryption
│   ├── audit/                 # audit_logs model + repository + Kafka
│   │                          # outbox + Gin handler
│   ├── admin/                 # Phase 2: source-management API,
│   │                          # per-source Redis rate limiter,
│   │                          # source-health tracker, forget worker
│   │                          # + Phase 4 simulator_handler.go
│   │                          # (/v1/admin/policy/...)
│   ├── policy/                # Phase 4: privacy modes, allow/deny
│   │                          # ACL, recipient policy + Phase 4
│   │                          # simulator (snapshot.go, simulator.go,
│   │                          # simulator_diff.go,
│   │                          # simulator_conflict.go), draft store
│   │                          # (draft.go), promotion FSM
│   │                          # (promotion.go, transactional audit via
│   │                          # AuditWriter.CreateInTx), GORM live
│   │                          # store (live_store.go) + live resolver
│   │                          # (live_resolver.go)
│   ├── pipeline/              # 4-stage pipeline (Phase 1):
│   │                          # consumer / coordinator / fetch / parse
│   │                          # / embed / store. Phase 2 adds
│   │                          # producer.go (partition-key routing) +
│   │                          # backfill.go (paced initial sync)
│   ├── retrieval/             # /v1/retrieve handler + parallel fan-out
│   │                          # merger / reranker / policy filter (Phase 3)
│   │                          # + Phase 4 PolicyResolver wiring
│   │                          # (policy_snapshot.go) + Phase 4
│   │                          # privacy strip enrichment
│   │                          # (privacy_strip.go)
│   ├── storage/               # Qdrant + Postgres + BM25 (bleve) +
│   │                          # FalkorDB + Redis semantic cache
│   ├── shard/                 # Phase 5: shard manifest API,
│   │                          # generation worker, delta sync,
│   │                          # cryptographic-forgetting orchestrator,
│   │                          # client contract (contract.go),
│   │                          # coverage endpoint, version-lookup
│   │                          # adapter, eviction policy (eviction.go)
│   ├── models/                # Phase 5: model catalog
│   │                          # (Bonsai-1.7B q4_0 / q8_0 / fp16) +
│   │                          # GET /v1/models/catalog handler
│   ├── b2c/                   # Phase 6: B2C client SDK bootstrap
│   │                          # (/v1/health, /v1/capabilities,
│   │                          # /v1/sync/schedule)
│   ├── observability/         # Phase 8: OpenTelemetry tracing helper
│   │                          # used by the pipeline + retrieval, plus
│   │                          # Prometheus metrics + Gin middleware
│   │                          # (metrics.go, middleware.go) scraped at
│   │                          # /metrics on cmd/api and cmd/ingest
│   ├── grpcpool/              # Phase 8: round-robin gRPC pool with
│   │                          # circuit breaker for the Python sidecars
│   ├── lifecycle/             # Phase 8: ordered shutdown runner used
│   │                          # by cmd/api + cmd/ingest
│   ├── config/                # Phase 8 / Round-4: startup config
│   │                          # validation (validate.go)
│   ├── eval/                  # Round-4 Task 1: retrieval evaluation
│   │                          # harness (Precision@K, Recall@K, MRR,
│   │                          # NDCG) + GET /v1/admin/eval/run
│   ├── errors/                # Round-4 Task 7: structured error
│   │                          # catalog + Gin middleware
│   ├── migrate/               # Phase 8: SQL migration runner backed
│   │                          # by schema_migrations (AUTO_MIGRATE)
│   └── util/                  # Internal helper packages shared by
│                              # admin + audit handlers (Round-12).
│                              # util/strutil holds the cursor /
│                              # pagination helpers that both
│                              # /v1/admin/audit and the admin list
│                              # endpoints reuse.
├── proto/
│   ├── docling/v1/            # Python Docling parsing service
│   ├── embedding/v1/          # Python embedding service
│   ├── graphrag/v1/           # Round-4 Task 2: GraphRAG entity
│   │                          # extraction (Stage 3b)
│   └── memory/v1/             # Mem0 persistent memory service
├── services/                  # Python ML microservices (Phase 3)
│   ├── _proto/                # generated Python gRPC stubs
│   ├── docling/               # Docling gRPC server + Dockerfile
│   ├── embedding/             # sentence-transformers gRPC server
│   ├── graphrag/              # Round-4 Task 2: GraphRAG gRPC server
│   ├── memory/                # Mem0 gRPC server + Dockerfile
│   └── gen_protos.sh          # regenerates _proto/ from proto/
├── migrations/                # SQL migrations 001..040 + per-migration
│                              # *.down.sql rollbacks (Round-4 Task 20)
│   └── rollback/              # Round-4 Task 20: per-migration
│                              # *.down.sql, applied via
│                              # `make migrate-rollback`
├── tests/
│   ├── e2e/                   # docker-compose smoke test (//go:build e2e):
│   │                          # smoke_test.go covers Phase 1 (pipeline
│   │                          # + retrieval + audit), phase234_test.go
│   │                          # covers Phase 2/3/4 (admin source CRUD,
│   │                          # ACL-deny via LiveResolverGORM, draft
│   │                          # promote/reject, simulator endpoints)
│   ├── integration/           # Go ↔ Python gRPC tests (//go:build integration)
│   ├── benchmark/             # pipeline + retrieval benchmarks
│   ├── regression/            # Round-4 Task 15: PR #12 regression
│   │                          # manifest + meta-tests
│   └── capacity/              # Phase 8 capacity test (`make capacity-test`)
├── docs/                      # PROPOSAL / ARCHITECTURE / PHASES / PROGRESS
│   │                          # / CUTOVER
│   ├── runbooks/              # Phase 7: per-connector ops runbooks
│   │                          # (credential rotation, quotas, outages,
│   │                          # error codes) — one Markdown per connector
│   └── contracts/             # Phase 5/6: client-side wire contracts
│                              # (uniffi-ios.md, uniffi-android.md,
│                              # napi-desktop.md,
│                              # local-first-retrieval.md,
│                              # bonsai-integration.md,
│                              # b2c-retrieval-sdk.md,
│                              # privacy-strip-render.md,
│                              # background-sync.md)
├── deploy/                    # Phase 8: HorizontalPodAutoscaler manifests
│                              # (hpa-api.yaml, hpa-ingest.yaml,
│                              # hpa-docling.yaml, hpa-embedding.yaml)
│                              # plus Round-4 Task 10 PrometheusRule
│                              # (alerts.yaml — `make alerts-check`)
├── docker-compose.yml         # Postgres / Redis / Kafka / Qdrant /
│                              # FalkorDB / Docling / embedding / memory
├── Makefile                   # build / test / lint / proto-gen /
│                              # test-e2e / test-integration / bench
├── scripts/                   # Round-13 Task 16/20:
│                              # doctor.sh (prereq checklist),
│                              # migrate-dry-run-pg.sh (PG dialect
│                              # check via disposable container)
└── .github/workflows/ci.yml   # CI: vet / test / lint / proto-check /
                               # e2e / services-unit / integration
```

The full target architecture (including phases that have not yet
landed) is documented in
[`docs/ARCHITECTURE.md` §9](docs/ARCHITECTURE.md#9-directory-structure).

---

## Documents in this repo

- [`docs/PROPOSAL.md`](docs/PROPOSAL.md) — product vision, capabilities,
  connector catalog, B2C, B2B, lifecycle, policy simulation, privacy,
  cross-platform strategy, device tiering, and the language-choice rationale
  for the Go context engine.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system overview, sync
  architecture, retrieval architecture, storage plane, multi-tenancy, and
  observability.
- [`docs/PHASES.md`](docs/PHASES.md) — phased delivery plan (Phase 0 through
  Phase 8) with exit criteria.
- [`docs/PROGRESS.md`](docs/PROGRESS.md) — checklist of what is built, what is
  in progress, and what is planned, including the Context Engine migration
  tasks.
- [`docs/CUTOVER.md`](docs/CUTOVER.md) — cutover plan and rollback procedure
  for the Python → Go context engine migration.
- [`docs/openapi.yaml`](docs/openapi.yaml) — OpenAPI 3.0 spec for the
  full public + admin HTTP surface (Phase 8 / Task 17).

---

## Repository conventions

- **Branches.** Use `devin/<timestamp>-<short-name>` for AI-generated work,
  `feat/<short-name>` for human-authored work, and `fix/<issue-number>` for
  bug fixes. Direct pushes to `main` are not permitted.
- **PR format.** PRs include a Summary, Plan / Phase reference, and a
  Verification section. Documentation-only PRs may skip Verification.
- **Documentation-as-code.** This repository is documentation-first.
  Architectural changes land here *before* the code change in the relevant
  service repository (`ai-agent-platform`, `ai-agent-context-engine`,
  `ai-agent-desktop`, `knowledge`, etc.).
- **CI lanes.** CI is split into a fast lane, a full lane, and a
  nightly lane (see
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml)).
  - **Fast lane** runs on every PR push and is required for merge.
    Round 14 (Task 15) split the legacy `fast-go` job into three
    parallel sub-jobs so the unit-test step no longer serialises
    the build. The full fast-lane roster is:
    `fast-check` (gofmt + vet),
    `fast-test` (race + cover),
    `fast-build` (cmd/ binaries),
    `fast-lint` (golangci-lint v2.5.0),
    `fast-eval` (golden corpus),
    `fast-alerts` (Prometheus alert/recording-rule YAML),
    `fast-rollback-parity` (migration ↔ rollback parity),
    `fast-migrate-dry-run` (SQLite dry-run),
    `fast-proto-check` (proto-gen drift), and
    `fast-python` (Python services unit tests). Each Go fast-lane
    job restores `~/.cache/go-build` via `actions/cache` (Round
    14 Task 16). Branch protection should require the single
    aggregator job — `Required CI (fast lane)` (`fast-required`)
    — which is the only check `needs:` every fast-lane job.
  - **Full lane** (`full-proto-gen`, `full-e2e`, `full-integration`,
    `full-connector-smoke`, `full-bench-e2e`, `full-capacity-test`,
    `full-migrate-dry-run-pg`) runs on push to `main`, PRs
    labelled `full-ci` (or `run-integration` for the integration
    job only), the nightly cron, and manual `workflow_dispatch`.
    Add the label when a PR touches the storage plane, the gRPC
    contracts, or anything else the fast lane can't cover.
  - **Nightly lane** (`nightly-fuzz`) runs `make fuzz` on the
    `27 6 * * *` cron so panics surface within 24 hours without
    blocking PR turnaround.
---

## Related repositories

| Repo | Role |
|---|---|
| `ai-agent-platform` | Go control-plane backend (Gin, GORM, PostgreSQL) — workspace, channels, knowledge nodes |
| `ai-agent-context-engine` | Source of the existing 4-stage pipeline; the Go rewrite lives alongside the Python ML microservices |
| `kennguy3n/knowledge` | Rust knowledge core (UniFFI / N-API) — on-device evidence store, decay, graph |
| `kennguy3n/llama.cpp` | On-device SLM runtime (Bonsai-1.7B GGUF) |
| `kennguy3n/slm-rich-media` | Cross-platform SLM / diffusion / VLM runtime |
| `kennguy3n/chat-b2b-skills` | B2B skill packs that consume the retrieval API |
| `kennguy3n/chat-b2c-skills` | B2C skill packs that consume the retrieval API |
| `kennguy3n/cv-guard` | On-device media safety classifier used in privacy gating |
| `kennguy3n/slm-guardrail` | On-device text safety classifier used in privacy gating |
