# hunting-fishball

> **Status.** Phases 0, 1, 2, 3, 4, 5, 6 (server-side), 7, and 8
> are in `main` (all 🟡 partial — see
> [`docs/PROGRESS.md`](docs/PROGRESS.md) for the live checklist).
> Round 6 (this PR) layers retrieval diversity (MMR), semantic
> deduplication, per-source embedding model selection, query
> expansion, chunk-level ACL, adaptive rate limiting, SSE
> streaming retrieval, API versioning, pipeline retry analytics,
> and 11 more admin surfaces on top of those phases.
> Phase 1 brings the Google Drive + Slack connectors, the Go Kafka
> consumer, the 4-stage pipeline (fetch / parse / embed / store), the
> `POST /v1/retrieve` API, and a docker-compose CI smoke test.
> **Phase 2** brings the admin source-management API
> (`internal/admin/`), per-tenant Kafka partition-key routing, the
> backfill-vs-steady pipeline (`pipeline.IngestEvent.SyncMode`), the
> per-source Redis token-bucket rate limiter, source-health tracking,
> and the forget-on-removal worker.
> Phase 3 brings the four-backend retrieval fan-out (vector + BM25 +
> graph + memory), the RRF merger and lightweight reranker, the
> Redis semantic cache, the three Python ML microservices (Docling,
> embedding, Mem0), Go ↔ Python integration tests, throughput /
> latency benchmarks, and the
> [cutover plan](docs/CUTOVER.md). **Phase 4** brings the policy
> framework (`internal/policy/`): privacy modes
> (`no-ai`/`local-only`/`local-api`/`hybrid`/`remote`), allow/deny
> ACL evaluation, and recipient policy — all wired into the retrieval
> handler via a `PolicyResolver` port — plus the policy simulator
> (what-if retrieval, data-flow diff, conflict detection), draft
> isolation with audited promotion (`policy.promoted` /
> `policy.rejected` audit events), and structured `privacy_strip`
> enrichment on every retrieval row. The admin HTTP surface lives at
> `/v1/admin/policy/{drafts,simulate,conflicts}`.
> **Phase 5 (server-side)** brings the on-device shard contract:
> the manifest API (`GET /v1/shards/:tenant_id`), the policy-aware
> generation worker, the delta sync protocol
> (`GET /v1/shards/:tenant_id/delta?since=<v>`), the shard
> coverage endpoint (`GET /v1/shards/:tenant_id/coverage`), the
> client `ShardClientContract` interface
> (`internal/shard/contract.go`) consumed by the iOS / Android /
> desktop runtimes, the per-tier eviction policy
> (`internal/shard/eviction.go`), and the cryptographic-forgetting
> orchestrator (`DELETE /v1/tenants/:tenant_id/keys`) — all in
> `internal/shard/`. The Bonsai-1.7B model catalog
> (q4_0 / q8_0 / fp16) ships in `internal/models/` with
> `GET /v1/models/catalog`. Per-platform contracts live in
> [`docs/contracts/`](docs/contracts/).
> **Phase 6 (server-side)** brings the B2C client SDK bootstrap
> (`internal/b2c/`): `GET /v1/health`, `GET /v1/capabilities`, and
> `GET /v1/sync/schedule`. The device-first policy
> (`internal/policy/device_first.go`) returns a
> `prefer_local` / `local_shard_version` / `prefer_local_reason`
> hint on every `RetrieveResponse`. Per-platform render and
> background-sync contracts live in
> [`docs/contracts/`](docs/contracts/).
> **Phase 7** brings ten new connectors (SharePoint, OneDrive,
> Dropbox, Box, Notion, Confluence, Jira, GitHub, GitLab, Microsoft
> Teams), bringing the production catalog to 12, plus per-connector
> ops runbooks (`docs/runbooks/`) and an end-to-end smoke suite
> (`tests/e2e/connector_smoke_test.go`,
> `make test-connector-smoke`).
> **Phase 8** brings OpenTelemetry tracing
> (`internal/observability/`), per-stage worker pools
> (`pipeline.StageConfig`), tunable Kafka rebalance, storage
> connection-pool sizing, a round-robin gRPC pool with circuit
> breaker (`internal/grpcpool/`), a capacity test harness
> (`tests/capacity/`, `make capacity-test`), Prometheus metrics
> + Gin middleware (`internal/observability/metrics.go`),
> four HPA manifests (`deploy/`), Mem0 tenant-prefix
> partitioning (`services/memory/memory_server.py::tenant_prefix`),
> liveness / readiness probes (`/healthz`, `/readyz`), and an
> end-to-end retrieval P95 budget enforcer
> (`tests/benchmark/p95_e2e_test.go` for the Phase 1 round-trip,
> `tests/benchmark/p95_retrieval_test.go` for the stricter Phase 3
> retrieval-only budget; `make bench-e2e`). 2026-05-10 also wired
> structured JSON logging through both binaries via
> `internal/observability/logger.go` (with `GinLoggerMiddleware`
> on the authed `cmd/api` route group; `cmd/ingest` uses
> `slog.SetDefault` since it serves probes via `net/http`),
> the DLQ observer (`internal/pipeline/dlq_observer.go`), and the
> 5-step `TenantDeleter` workflow
> (`internal/admin/tenant_delete.go`,
> `DELETE /v1/admin/tenants/:tenant_id`) backed by
> `migrations/008_tenant_status.sql`. The channel
> `deny_local_retrieval` flag is now end-to-end
> (`migrations/007_channel_deny_local.sql`).
> The product thesis lives in
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
| `make fuzz`             | Run Go native fuzz targets in `internal/retrieval/` (30s)   |
| `make migrate-rollback` | Apply `migrations/rollback/*.down.sql` in reverse via `psql` (gated on `CONTEXT_ENGINE_DATABASE_URL`) |
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
│   └── migrate/               # Phase 8: SQL migration runner backed
│                              # by schema_migrations (AUTO_MIGRATE)
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
├── migrations/                # SQL migrations (audit_logs, ...)
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
- **CI lanes.** CI is split into a fast lane and a full lane (see
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml)).
  - **Fast lane** (gofmt, vet, golangci-lint, race+cover unit tests, build,
    Python services unit tests) runs on every PR push and is required for
    merge. Branch protection should require the `Required CI (fast lane)`
    aggregator check.
  - **Full lane** (proto-gen check, e2e smoke against the docker-compose
    stack, Go ↔ Python integration with the heavy ML images) runs on:
    push to `main`, PRs labelled `full-ci` (or `run-integration` for the
    integration job only), the nightly `27 6 * * *` cron, and manual
    `workflow_dispatch`. Add the label when a PR touches the storage
    plane, the gRPC contracts, or anything else that the fast lane can't
    cover.
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
