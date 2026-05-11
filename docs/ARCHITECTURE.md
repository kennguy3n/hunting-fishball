# hunting-fishball — Architecture

> **Audience.** Engineers building or operating the platform.
>
> **Status.** Greenfield. This document describes the *target* architecture.
> Tracked deliverables are in [`PHASES.md`](PHASES.md); status is in
> [`PROGRESS.md`](PROGRESS.md).

---

## 1. System overview

```
                 ┌────────────────────────────────────────────────┐
                 │                  Clients                       │
                 │  Admin Portal (React)                          │
                 │  iOS    (Swift / SwiftUI + UniFFI)             │
                 │  Android (Kotlin / Compose + UniFFI)           │
                 │  Desktop (Electron + React + N-API)            │
                 └─────────────────┬──────────────────────────────┘
                                   │ REST / WS / SSE
                                   ▼
                 ┌────────────────────────────────────────────────┐
                 │  Platform Backend  (Go — Gin, GORM, Postgres)  │
                 │   Tenancy / IAM / billing / audit              │
                 │   Connector registry + source mapping          │
                 │   Workspace / channel / knowledge node CRUD    │
                 │   Policy authoring + simulator API             │
                 └─────────────┬───────────────┬──────────────────┘
                               │ Kafka         │ HTTP (Gin)
                               ▼               ▼
   ┌──────────────────────────────────┐   ┌──────────────────────────────────┐
   │  Context Engine  (Go)            │   │  Retrieval API  (Go — Gin)       │
   │   Kafka consumer (sarama /       │   │   Vector  (Qdrant Go client)     │
   │     confluent-kafka-go)          │   │   BM25    (tantivy-go)           │
   │   Pipeline coordinator           │   │   Graph   (FalkorDB Go client)   │
   │     (goroutines + channels)      │   │   Memory  (gRPC → Mem0 service)  │
   │   Stage 1 Fetch  (Go)            │◀──│   Result merger + reranker       │
   │   Stage 2 Parse  (gRPC → Python) │   │   Semantic cache (Redis)         │
   │   Stage 3 Embed  (gRPC → Python  │   └──────────────────────────────────┘
   │                  or remote API)  │
   │   Stage 4 Storage (Go)           │
   └──────────────────┬───────────────┘
                      │ gRPC
                      ▼
   ┌────────────────────────────────────────────────────────────────────────┐
   │  Python ML Microservices  (stateless workers, gRPC)                    │
   │   Docling parsing service     — PDF / DOCX / XLSX / PPTX / HTML / EPUB │
   │   Embedding service           — local model inference + batching       │
   │   GraphRAG entity extractor   — entity / relation extraction           │
   │   Mem0 memory service         — persistent user / session memory       │
   └─────────────────────────────────┬──────────────────────────────────────┘
                                     │
                                     ▼
   ┌────────────────────────────────────────────────────────────────────────┐
   │  Storage plane                                                         │
   │   PostgreSQL  (metadata / audit / policy)                              │
   │   Qdrant      (vectors, per-tenant collections)                        │
   │   FalkorDB    (knowledge graph, per-tenant graphs)                     │
   │   Tantivy     (BM25 indexes, per-tenant directories)                   │
   │   Redis       (semantic cache, rate limit, distributed locks)          │
   │   Object store (raw artifacts, parsed text, sharded indices)           │
   └────────────────────────────────────────────────────────────────────────┘
                                     ▲
                                     │
   ┌────────────────────────────────────────────────────────────────────────┐
   │  On-device tier                                                        │
   │   Knowledge Core (Rust)            — UniFFI / N-API surface            │
   │   Bonsai-1.7B SLM (llama.cpp)      — CPU / Metal / CUDA / Vulkan / NPU │
   │   On-device retrieval shards       — synced subset of remote indices   │
   │   Local redaction & privacy strip                                      │
   └────────────────────────────────────────────────────────────────────────┘
```

The Go context engine is shown as a single component but is deployed as
two scaling units in production: an *ingestion* deployment (Kafka consumer
+ pipeline) and an *API* deployment (Gin retrieval). They share libraries,
config, and observability instrumentation, but scale independently —
ingestion scales on Kafka lag; the API scales on QPS.

The Python ML microservices are shown as a logical group, but each
service is its own Kubernetes deployment with its own HPA. They share
nothing — the Docling service can be at 80 % CPU while the embedding
service is idle.

---

## 2. Components

### 2.1 Platform Backend (Go)

The control plane. Owns tenancy, IAM, billing, the connector registry,
source mapping, the workspace / channel / knowledge-node hierarchy, the
audit log, and the policy authoring surface.

- **Stack.** Go, Gin, GORM, PostgreSQL, Redis, Kafka, Keycloak (OIDC).
- **Identity.** Multi-tenant ULID keys; `tenant_id` injected into every
  request via OIDC token claims and propagated through Postgres RLS GUCs.
- **Connector registry.** Process-global registry; connectors blank-import
  themselves for `init()` registration.
- **Audit log.** Append-only PostgreSQL table backed by an outbox into
  Kafka for downstream observability.
- **Public surface.** REST (Gin) for the admin portal and clients; gRPC
  for the context engine and agent runtimes.

The platform backend never talks to vector / graph / lexical stores
directly. It mediates everything through the context engine and retrieval
API.

### 2.2 Context Engine (Go)

The data-plane orchestrator. Consumes ingestion events from Kafka, runs
the 4-stage pipeline, and writes derived state to the storage plane.

- **Stack.** Go, sarama or confluent-kafka-go, Gin (for the retrieval API
  half), `errgroup`, OpenTelemetry, Redis, Qdrant Go client, FalkorDB Go
  client, `tantivy-go`.
- **Concurrency model.** Goroutines per Kafka partition; bounded
  goroutine pools per pipeline stage; channels for back-pressure between
  stages; `errgroup` for retrieval fan-out.
- **Failure model.** Per-document retries with exponential backoff;
  poison-message DLQ; idempotent stage outputs keyed on
  `(tenant_id, document_id, content_hash)`.
- **Deployment.** Two deployments share the codebase: `context-engine-ingest`
  (Kafka consumer + pipeline) and `context-engine-api` (Gin retrieval).

### 2.3 Python ML Microservices

Stateless workers behind gRPC interfaces. Each service is its own
Kubernetes deployment.

- **Docling parsing service** — wraps Docling for PDF / DOCX / XLSX /
  PPTX / HTML / EPUB. Returns parsed blocks + structure markers (heading
  level, table cells, image captions). Stateless; no per-tenant state.
- **Embedding service** — local model inference (sentence-transformers
  family) with batching. Stateless; receives `[]string` chunks, returns
  `[][]float32`. The Go orchestrator picks per-tenant whether to call
  this service or a remote embedding API.
- **GraphRAG entity extractor** — entity + relation extraction over
  parsed chunks. Returns nodes / edges in a stable schema for the Go
  Stage 4 worker to write to FalkorDB.
- **Mem0 memory service** — persistent user / session memory. Receives
  `WriteMemory` and `SearchMemory` calls; persists to its own backing
  store (PostgreSQL + per-user partitions).

The Go orchestrator owns *all* state and *all* tenancy enforcement. The
Python services see only the data the Go orchestrator chose to send and
the tenant-scoped headers.

### 2.4 Storage plane

| Store | Purpose | Tenancy |
|---|---|---|
| PostgreSQL | Metadata, audit log, policy, source mapping | Per-tenant RLS via `app.tenant_id` GUC |
| Qdrant | Vector embeddings | Per-tenant collection |
| FalkorDB | Knowledge graph (entity / relation) | Per-tenant graph |
| Tantivy | BM25 / hybrid lexical indexes | Per-tenant directory |
| Redis | Semantic cache, rate limit, distributed locks | Per-tenant key prefix |
| Object store | Raw artifacts, parsed text blobs, index shards | Per-tenant bucket prefix |

Tenant isolation is enforced at the **library boundary** — the Go
storage clients refuse to issue a query without a non-empty
`tenant_scope`, and propagate it through the protocol-specific isolation
mechanism (RLS GUC, collection name, graph name, directory path, key
prefix, bucket prefix).

### 2.5 On-device tier

The on-device tier is owned by `kennguy3n/knowledge` (Rust), exposed to
mobile via UniFFI and to desktop via N-API.

- **Encrypted evidence store** — per-user, per-device.
- **Decay state machine** — Candidate → Canonical → Deleted.
- **On-device concept graph** — sparse typed graph for local reasoning.
- **On-device retrieval shards** — synced subset of remote indices,
  scoped to the user / channel / privacy mode.
- **Bonsai-1.7B SLM via `llama.cpp`** — CPU / Metal / CUDA / Vulkan /
  NPU as the device supports.

Clients always try local retrieval *first* when policy allows; the
remote retrieval API is contacted only when the on-device tier reports
insufficient coverage or the channel policy demands it.

---

## 3. Sync architecture

The sync architecture is what turns "an admin connected a SharePoint" into
"the user's query against that SharePoint returns ranked, governed
results in 250 ms".

### 3.1 Event flow

```
Source system  ──▶  Connector  ──▶  Kafka topic  ──▶  Go context engine
                       │                                  │
                       │ source.document_changed          │ goroutine per partition
                       │                                  │
                       │                                  ▼
                       │                            Pipeline coordinator
                       │                            ┌────────────────────┐
                       │                            │ Stage 1: Fetch  Go │
                       │                            │ Stage 2: Parse  gRPC ─▶ Docling
                       │                            │ Stage 3: Embed  gRPC ─▶ Embedding (or remote)
                       │                            │ Stage 4: Store  Go    │
                       │                            └────────────────────┘
                       │                                  │
                       │                                  ▼
                       │                            Storage plane
                       │                            (Qdrant + FalkorDB +
                       │                             Tantivy + Postgres)
                       │
                       └──▶ outbox / audit (platform backend)
```

### 3.2 Kafka consumer (Go)

- One topic per *workload* (ingest, reindex, purge); per-tenant routing
  via partition key (`tenant_id || source_id`).
- Consumer group rebalancing tuned for sticky assignment so per-source
  ordering is preserved across rebalance.
- Goroutine per partition; bounded in-flight per partition to avoid
  unbounded back-pressure into the pipeline.
- Offsets committed only after Stage 4 completes; failures route to a
  DLQ topic with the original payload + failure metadata.

This replaces the previous Python `FastKafkaConsumer` worker pool. The
Go consumer has no GIL contention and no `multiprocessing` overhead.

### 3.3 Pipeline coordinator (Go)

The coordinator turns a `SourceDocumentChanged` event into a sequence of
stage calls. It uses **goroutines and channels** (replacing Python's
`multiprocessing` `ProcessCoordinator`) so stages can pipeline:

```go
type StageInput  struct { Doc *Document; ParsedBlocks []Block; Embeddings [][]float32 }
type StageOutput struct { Err error; Out *StageInput }

g, ctx := errgroup.WithContext(ctx)
fetchCh, parseCh, embedCh, storeCh := make(chan *Document), make(chan *Document), make(chan *StageInput), make(chan *StageInput)

g.Go(func() error { return runFetch(ctx, in, fetchCh) })
g.Go(func() error { return runParse(ctx, fetchCh, parseCh, doclingClient) })
g.Go(func() error { return runEmbed(ctx, parseCh, embedCh, embedClient) })
g.Go(func() error { return runStore(ctx, embedCh, storeCh, qdrant, falkor, pg) })
```

- **Stage 1: Fetch (Go).** HTTP / S3 downloads. Retry, dedupe, content-hash
  short-circuit (`if hash == previous_hash, skip downstream stages`).
- **Stage 2: Parse (gRPC → Docling).** Single gRPC call per document.
  Docling returns parsed blocks; the Go side wraps them in tenant /
  source / privacy metadata.
- **Stage 3: Embed (gRPC → embedding service, or remote API).** Per-tenant
  config picks the path. Local-only tenants go to the local Python
  service; tenants on a hosted plan can route to a remote API.
- **Stage 4: Storage (Go).** Writes to Qdrant (vectors), FalkorDB
  (graph), Tantivy (lexical), and PostgreSQL (metadata). Writes are
  batched; commits go through a transactional outbox so partial failures
  do not leave dangling state.
- **Stage 3b: Entity extraction (gRPC → GraphRAG, optional, Round-4
  Task 2).** When `CONTEXT_ENGINE_GRAPHRAG_ENABLED=true`, the
  coordinator hooks `internal/pipeline/graphrag.go` between Embed
  and Store. The GraphRAG service in `services/graphrag/` returns
  nodes + edges extracted from the parsed blocks; the coordinator
  writes them to FalkorDB through `internal/storage/falkordb.go`
  alongside the Stage 4 vector writes. This stage is opt-in and
  off by default — pipelines without graph requirements pay zero
  overhead.

### 3.4 Idempotency

Every stage output is keyed on `(tenant_id, document_id, content_hash)`.
Re-processing a document with the same content produces no net change to
the storage plane; re-processing with new content updates exactly the
chunks that changed. This is what makes paced backfills, recovery from
DLQ, and connector restarts safe.

### 3.5 Backfill vs. steady state

- **Backfill.** Triggered by an admin connecting a new source. The
  connector emits documents at a paced rate; the Go consumer groups
  events into a "backfill" partition stream so steady-state ingestion is
  not starved.
- **Steady state.** Webhook / delta token events flow at the source's
  natural rate. The pipeline coordinator can keep up because Stages 1
  and 4 are pure Go.

---

## 4. Retrieval architecture

### 4.1 API surface

```
POST /v1/retrieve
{
  "tenant_id":      "<ULID>",
  "user_id":        "<ULID>",
  "channel_id":     "<ULID | null>",
  "query":          "<string>",
  "scope":          { "sources": [...], "privacy_mode": "hybrid" },
  "limits":         { "k": 20, "max_latency_ms": 500 }
}

→ 200 OK
{
  "results": [
    {
      "chunk_id": "...",
      "source": { "connector": "google_drive", "uri": "..." },
      "score": 0.873,
      "privacy_label": "hybrid:remote_embedding",
      "provenance": { "tenant_id": "...", "ingested_at": "..." },
      "text": "..."
    }
  ],
  "policy":  { "applied": [...], "blocked_count": 0 },
  "timings": {
    "vector_ms": 12, "bm25_ms": 8, "graph_ms": 0,
    "memory_ms": 0,  "merge_ms": 1, "rerank_ms": 1
  },
  "trace_id": "..."
}
```

The same shape is returned on every platform. The privacy label is the
client's source of truth for the privacy strip UI. The `timings`
block breaks the request's wall-clock down by backend so operators
can identify the long pole without reaching for a trace; values are
0 when the corresponding backend is not configured.

**Bulk retrieval (Phase 8 / Task 12).** Clients populating dashboards
or running multi-query experiments use `POST /v1/retrieve/batch` to
fan out up to 32 sub-requests at once. The handler caps in-flight
work at `max_parallel` (default 8); per-request policy and privacy
isolation is preserved so one failed query does not fail the batch.
Schema: see `docs/openapi.yaml` (BatchRequest / BatchResponse).

**Admin surface (Phase 8 batch).** The admin handlers are grouped
under `/v1/admin/`:

- `POST /v1/admin/sources`, `GET`, `PATCH`, `DELETE`,
  `GET /v1/admin/sources/:id/health` — connector CRUD + health.
- `GET /v1/admin/sources/:id/progress` — per-namespace sync progress
  (`internal/admin/sync_progress_handler.go`, Task 14).
- `GET /v1/admin/dashboard` — tenant-wide health + throughput +
  P95 + per-backend availability summary
  (`internal/admin/dashboard_handler.go`, Task 19).
- `GET /v1/admin/dlq`, `GET /v1/admin/dlq/:id`,
  `POST /v1/admin/dlq/:id/replay` — dead-letter inspection + retry
  (`internal/admin/dlq_handler.go`, Task 5).
- `POST /v1/admin/reindex` — re-emit Stage 2–4 events for an
  existing tenant / source / namespace
  (`internal/admin/reindex_handler.go`, Task 7).
- `POST /v1/admin/policy/drafts` (+ list / get / promote / reject /
  simulate / simulate/diff / conflicts) — policy framework.
- `DELETE /v1/admin/tenants/:tenant_id` — tenant deletion workflow.
- `GET /v1/admin/audit` (also reachable as `/v1/audit-logs` for
  back-compat) — audit log search/filter with `action=` /
  `resource_id=` / `source_id=` / `q=` / `since=` / `until=`
  (`internal/audit/handler.go`, Task 13).

**Public-API rate limit (Phase 8 / Task 8).** A Gin middleware on
the `/v1/` group enforces a per-tenant token bucket backed by Redis
when `CONTEXT_ENGINE_API_RATE_LIMIT` is set; HTTP 429 + `Retry-After`
on overage. Falls open on Redis failure so a transient cache outage
does not blackhole the API.

**Request ID middleware (Phase 8 / Task 20).** Every inbound request
either echoes a sanitised `X-Request-ID` or has one minted as a ULID.
The ID is bound to the gin context (`request_id`), the request
context, the response header, and the per-request `slog` logger so
every downstream log line and trace span correlates.

**Operational endpoints.** Each binary (`cmd/api`, `cmd/ingest`)
additionally exposes:

- `GET /healthz` — liveness probe; always 200 if the process is
  running. Used by Kubernetes `livenessProbe`.
- `GET /readyz` — readiness probe. The API binary checks Postgres
  + Redis + Qdrant; the ingest binary checks Postgres + Redis +
  TCP connectivity to every Kafka broker. Returns 503 + the failed
  dependency name if any check fails. Used by Kubernetes
  `readinessProbe` so traffic is only routed once dependencies are
  reachable.
- `GET /metrics` — Prometheus scrape endpoint. Collectors are
  defined in `internal/observability/metrics.go`; the API binary
  layers a Gin middleware
  (`internal/observability/middleware.go`) that records request
  count + duration per route. The ingest binary records Kafka
  consumer lag from `internal/pipeline/consumer.go` after every
  commit, per-stage pipeline duration from
  `internal/pipeline/coordinator.go::runWithRetry`, and the
  retrieval handler records per-backend duration in
  `internal/retrieval/handler.go::fanOut`.

The Python ML sidecars (`services/docling`, `services/embedding`)
expose `/metrics` on a separate sidecar HTTP listener (default
port 9090, override via `METRICS_PORT`). Their queue-depth and
request-duration collectors are defined in the shared
[`services/_metrics.py`](../services/_metrics.py) module.

### 4.2 Parallel fan-out

The retrieval API runs four backends **in parallel** with goroutines and
`errgroup`:

```go
g, ctx := errgroup.WithContext(ctx)
var vec, bm25, graph, mem []*Match

g.Go(func() error { var err error; vec,   err = qdrant.Search(ctx, ...);   return err })
g.Go(func() error { var err error; bm25,  err = tantivy.Search(ctx, ...);  return err })
g.Go(func() error { var err error; graph, err = falkor.Traverse(ctx, ...); return err })
g.Go(func() error { var err error; mem,   err = mem0.Search(ctx, ...);     return err })

if err := g.Wait(); err != nil { /* partial-result fallback per policy */ }

merged := merger.Merge(vec, bm25, graph, mem)
ranked := reranker.Rerank(ctx, merged)
```

Each backend has its own deadline (a fraction of `max_latency_ms`); if a
backend misses its deadline, retrieval degrades to whatever finished in
time and the `policy` field records the degradation.

### 4.3 Result merger and reranker

- **Merger.** Reciprocal rank fusion across the four streams, weighted
  per channel policy.
- **Reranker.** Optional cross-encoder rerank when the channel allows
  remote compute; otherwise a Go-side lightweight reranker (BM25 score
  blending + freshness boost).
- **Policy filter.** After merge / rerank, the result list is filtered
  by tenant policy: chunks whose `privacy_label` exceeds the channel's
  privacy mode are dropped (and counted in `policy.blocked_count`).

### 4.4 Semantic cache

- Cache key: hash of `(tenant_id, channel_id, query_embedding,
  scope_hash)`.
- Cache value: top-k merged + reranked result list with expiry.
- Stored in Redis with a per-tenant key prefix.
- Cache is consulted *before* the fan-out; on hit, the API returns
  immediately.

The cache is invalidated lazily — when a Stage 4 storage write touches a
chunk that participates in a cached entry, the entry is dropped.

### 4.5 Evaluation harness

PROPOSAL.md §1 lists "an evaluation harness that knows whether
retrieval is actually any good" as one of the 5 core capabilities.
The harness lives in `internal/eval/` and is exposed through the
admin API.

- **Suite (`internal/eval/eval.go`).** An `EvalSuite` is a
  named collection of `(query, expected_chunk_ids,
  expected_min_score)` tuples. Suites are scoped per-tenant
  and persisted in the `eval_suites` table
  (`migrations/012_eval_suites.sql`). The corpus is stored as
  JSONB so a suite can be edited without a schema migration.
- **Metrics (`internal/eval/metrics.go`).** Pure functions that
  take a `[]RetrieveHit` plus the expected chunk IDs and return:
  - **Precision@K** — fraction of the top-K hits that are in the
    expected set.
  - **Recall@K** — fraction of the expected set that appears in
    the top-K hits.
  - **MRR** — mean reciprocal rank over the eval queries.
  - **NDCG@K** — discounted gain weighting earlier-positioned
    relevant hits.
- **Runner (`internal/eval/runner.go`).** Walks the suite,
  drives the retrieval handler for each query, and aggregates the
  per-query metrics into an `EvalReport`. The handler is passed
  in as a function pointer so the runner is unit-testable with a
  fake retrieval implementation.
- **Admin handler (`internal/eval/handler.go`).**
  `GET /v1/admin/eval/run` mounts the runner; an admin can hit
  the endpoint for a one-shot regression suite at deploy time.

The harness is what gates "is retrieval still any good after
that change to ACL evaluation / reranker / embedding model?"
in pre-deploy CI.

---

## 5. Multi-tenancy

Tenancy is enforced at three layers:

1. **API gateway.** Every request carries an OIDC token; the gateway
   resolves `tenant_id` and injects it into the request context.
2. **Storage clients.** The Go storage clients refuse calls without a
   non-empty `tenant_scope`; the scope is propagated into the
   protocol-specific isolation mechanism (Postgres RLS GUC, Qdrant
   collection name, FalkorDB graph name, Tantivy directory, Redis prefix,
   bucket prefix).
3. **Audit log.** Every retrieval and ingestion event records its
   `tenant_id`. Cross-tenant queries are *structurally impossible* at
   the storage-client layer — they cannot construct a query that
   addresses two tenants at once.

A tenant deletion is a multi-stage workflow:

1. Mark tenant `pending_deletion` in PostgreSQL; refuse new ingestion.
2. Drain in-flight pipeline messages; commit final offsets.
3. Drop Qdrant collections, FalkorDB graphs, Tantivy directories, Redis
   keys, object-store prefixes — in that order.
4. Cryptographically destroy any per-tenant data-encryption keys.
5. Mark tenant `deleted`.

---

## 6. Observability

Every component emits **OpenTelemetry** traces, metrics, and logs.

- **Traces.** Each retrieval call gets a single `trace_id` that spans:
  API gateway → retrieval API → 4 parallel backend spans → merger →
  reranker → response. gRPC calls into the Python tier propagate the
  trace.
- **Metrics.** Per-tenant ingestion lag, per-stage latency, per-backend
  retrieval latency, per-tier policy block counts, Kafka consumer lag,
  Python service CPU / RAM, embedding QPS. The Go binaries register
  the following Prometheus collectors
  (`internal/observability/metrics.go`):

  | Name | Type | Labels | Owner |
  |------|------|--------|-------|
  | `context_engine_api_requests_total` | counter | method, path, status | API HPA QPS metric |
  | `context_engine_api_request_duration_seconds` | histogram | method, path | API SLOs |
  | `context_engine_kafka_consumer_lag` | gauge | topic, partition, consumer_group | Ingest HPA backlog metric |
  | `context_engine_pipeline_stage_duration_seconds` | histogram | stage | Pipeline regression detection |
  | `context_engine_retrieval_backend_duration_seconds` | histogram | backend | Long-pole detection |
  | `context_engine_retrieval_backend_hits` | gauge | backend | Backend health |

  The Python sidecars expose a per-prefix triplet (`<prefix>_requests_total`,
  `<prefix>_duration_seconds`, `<prefix>_queue_depth`) defined by
  `services/_metrics.py::ServiceMetrics`. Concrete prefixes are
  `docling_parse` and `embedding`.

- **HPA wiring.** Four `HorizontalPodAutoscaler` manifests under
  [`deploy/`](../deploy/) target the metrics above:

  | Manifest | Scales | On |
  |----------|--------|----|
  | `deploy/hpa-api.yaml` | `context-engine-api` | CPU 70% + `context_engine_api_requests_per_second` |
  | `deploy/hpa-ingest.yaml` | `context-engine-ingest` | CPU 70% + `context_engine_kafka_consumer_lag` |
  | `deploy/hpa-docling.yaml` | `docling` | CPU 80% + memory 80% + `docling_parse_queue_depth` |
  | `deploy/hpa-embedding.yaml` | `embedding` | CPU 80% + memory 85% + `embedding_queue_depth` |

- **Logs.** Structured JSON; every line carries `tenant_id`, `trace_id`,
  `component`. PII is redacted at the log line by the logger middleware.
- **Per-connector runbooks.** Operations runbooks for every Phase 7
  connector live in [`docs/runbooks/`](runbooks/) — one Markdown
  file per connector covering credential rotation, quota /
  rate-limit incidents, outage detection, and connector-specific
  error codes. Each runbook is keyed on the connector's auth model
  (OAuth, API token, service account, app passwords) and the delta
  cursor / change-feed mechanism it relies on.

Dashboards are owned by SRE and stored in the platform backend's
`observability/` overlay; runbooks reference dashboard URLs by stable ID.

---

## 7. Security and key management

- **Credentials at rest.** Connector credentials are envelope-encrypted
  with a per-tenant DEK; the DEK is wrapped by a KMS-managed CMK.
- **In-flight.** All inter-service traffic is mTLS (gRPC) or TLS 1.3
  (HTTP). Kafka uses SASL_SSL.
- **At rest in storage.** PostgreSQL TDE; Qdrant and Tantivy data dirs
  on encrypted volumes; object-store SSE-KMS.
- **On-device.** The Rust knowledge core uses XChaCha20-Poly1305 with
  per-tenant keys stored in the OS keychain (Keychain on iOS / macOS,
  Keystore on Android, DPAPI on Windows, `libsecret` on Linux).
- **Forgetting.** Cryptographic destruction of per-tenant keys renders
  any residual snapshots unreadable.

### 7.1 Webhook security (Round-4 Task 3)

Connectors that implement `WebhookReceiver` (GitHub, GitLab,
Jira, Teams, Slack) validate the originating webhook before
trusting any payload. The shared validator lives in
`internal/connector/webhook_verify.go`:

```go
func VerifyHMAC(secret, payload []byte, signature, algo string) error
```

Each connector funnels its provider-specific scheme into this
function:

| Provider | Header | Algorithm |
|----------|--------|-----------|
| GitHub | `X-Hub-Signature-256` | HMAC-SHA256 hex |
| GitLab | `X-Gitlab-Token` | shared-secret compare |
| Jira | `X-Atlassian-Webhook-Signature` | HMAC-SHA256 |
| Teams | `Authorization: Bearer …` | shared-secret compare |
| Slack | `X-Slack-Signature` + `X-Slack-Request-Timestamp` | HMAC-SHA256 with replay window |

Verification fails-closed: an unsigned or mis-signed payload
returns 401 before any storage or audit side-effects. Per-
connector unit tests cover both valid and invalid signatures
plus replay-window edges where applicable.

### 7.2 Connector credential rotation (Round-4 Task 16)

`POST /v1/admin/sources/:id/rotate-credentials`
(`internal/admin/credential_rotation.go`) accepts a new
credential blob, runs the connector's `Validate` against it
*before* mutating any state, then atomically swaps:

- new credentials replace the source's `Credentials` field;
- previous credentials are stashed under `previous_credentials`
  with an `expiry_at` set `CredentialGracePeriod` (default 1h)
  in the future, so in-flight requests holding the old
  credential drain naturally;
- a `source.credentials_rotated` audit row is emitted with
  actor, reason, and the grace-period expiry.

A rotation that fails Validate leaves every field untouched —
no half-rotated state.

---

## 8. Failure modes and degradation

| Failure | Behavior |
|---|---|
| Python Docling service down | Stage 2 retries with backoff; documents pile in the parse queue; Stage 1 throttles. Retrieval is unaffected. |
| Python embedding service down | Stage 3 routes to remote API if the tenant allows it; otherwise queues. Retrieval is unaffected. |
| Qdrant unreachable | Retrieval drops vector matches; merger uses BM25 + graph + memory. `policy.degraded=true`. |
| FalkorDB unreachable | Same — drop graph matches. |
| Tantivy unavailable | Same — drop BM25 matches. |
| Redis cache down | Bypass cache; full fan-out per query. |
| Kafka cluster down | Connectors buffer events locally up to a configured limit; ingestion stops; retrieval over already-indexed content is unaffected. |
| Platform backend down | All ingestion and retrieval stops at the API gateway. |

The retrieval API never silently returns an empty result for a
degradation. It returns the best result set it could compute, with a
`policy.degraded` flag and a structured `policy.applied` list explaining
which backends contributed.

Phase 8 / Task 16 ships `tests/e2e/degradation_test.go`, exercising
the four canonical failure modes in-process (vector down, all
backends down, slow backend exceeding the budget, and Redis cache
down). The retrieval handler is asserted to never return a 5xx and
to always carry `policy.degraded` for any backend that errored.

**Graceful shutdown (Phase 8 / Task 10).** Both binaries trap
`SIGTERM` / `SIGINT` and run an ordered `lifecycle.Step` sequence:

- `cmd/api`: `http-server` (drain in-flight requests) → `redis` →
  `postgres`.
- `cmd/ingest`: `kafka-consumer` (stop polling, commit offsets) →
  `pipeline-coordinator` (drain in-flight stages) → `http-server`
  (close health probe listener) → `postgres` → `redis`.

The shutdown deadline is `CONTEXT_ENGINE_SHUTDOWN_TIMEOUT_SECONDS`
(default 15s for `cmd/api`, 30s for `cmd/ingest`); steps that
exceed the budget log the timeout and the runner moves on so a
single hung resource doesn't block process exit.

---

## 9. Directory structure

The code layout below is what the Phase 0 scaffolding lands. Each
package has a single owner contract (interface + tests) so future phases
can grow inside the same tree without restructuring.

```
hunting-fishball/
├── cmd/
│   ├── ingest/                # context-engine-ingest binary entry point
│   └── api/                   # context-engine-api    binary entry point
├── internal/
│   ├── connector/             # SourceConnector interface, optional
│   │   │                      # interfaces (DeltaSyncer / WebhookReceiver /
│   │   │                      # Provisioner), process-global registry
│   │   ├── googledrive/       # Google Drive connector (Phase 1)
│   │   ├── slack/             # Slack connector + Events API (Phase 1)
│   │   ├── sharepoint/        # SharePoint Online (Phase 7, Graph delta)
│   │   ├── onedrive/          # OneDrive personal (Phase 7, Graph delta)
│   │   ├── dropbox/           # Dropbox v2 (Phase 7, list_folder cursor)
│   │   ├── box/               # Box (Phase 7, events stream)
│   │   ├── notion/            # Notion (Phase 7, last_edited_time)
│   │   ├── confluence/        # Confluence Cloud (Phase 7, CQL delta)
│   │   ├── jira/              # Jira Cloud (Phase 7, JQL + webhooks)
│   │   ├── github/            # GitHub issues / PRs (Phase 7, webhooks)
│   │   ├── gitlab/            # GitLab issues (Phase 7, webhooks)
│   │   └── teams/             # Microsoft Teams (Phase 7, Graph delta +
│   │                          # change notifications)
│   ├── credential/            # AES-256-GCM envelope encryption for
│   │                          # connector credentials
│   ├── audit/                 # AuditLog model, repository (transactional
│   │                          # outbox), Kafka outbox poller, Gin handler
│   ├── admin/                 # Phase 2: source-management API
│   │                          # (handler / repo / model), per-source
│   │                          # Redis token-bucket rate limiter,
│   │                          # source-health tracker, forget-on-
│   │                          # removal worker. Phase 4 added
│   │                          # simulator_handler.go which mounts
│   │                          # /v1/admin/policy/{drafts,simulate,
│   │                          # conflicts}
│   ├── policy/                # Phase 4: privacy modes
│   │                          # (privacy_mode.go), allow / deny ACLs
│   │                          # (acl.go), recipient policy
│   │                          # (recipient.go) — wired into
│   │                          # internal/retrieval via
│   │                          # policy_snapshot.go. Phase 4
│   │                          # simulator: snapshot.go (port +
│   │                          # PolicySnapshot.Clone), simulator.go
│   │                          # (what-if), simulator_diff.go
│   │                          # (data-flow diff), simulator_conflict.go
│   │                          # (conflict detection), draft.go +
│   │                          # promotion.go (audited promotion FSM,
│   │                          # AuditWriter.CreateInTx so the audit
│   │                          # row rides the outer tx),
│   │                          # live_store.go (transactional GORM
│   │                          # writes to migrations/004_policy.sql
│   │                          # tables) + live_resolver.go (the read
│   │                          # counterpart used by both retrieval
│   │                          # and the simulator)
│   ├── pipeline/              # 4-stage pipeline (Phase 1):
│   │                          # consumer / coordinator / fetch / parse
│   │                          # / embed / store. Phase 2 added
│   │                          # producer.go (per-tenant partition-key
│   │                          # routing) + backfill.go (paced
│   │                          # initial sync, IngestEvent.SyncMode)
│   ├── retrieval/             # POST /v1/retrieve handler (Phase 1)
│   │                          # + parallel fan-out merger / reranker
│   │                          # / policy filter (Phase 3:
│   │                          # merger.go, reranker.go,
│   │                          # storage_adapter.go) + Phase 4 policy
│   │                          # snapshot resolver (policy_snapshot.go)
│   │                          # + Phase 4 privacy strip enrichment
│   │                          # (privacy_strip.go)
│   ├── storage/               # Qdrant + Postgres storage clients
│   │                          # (Phase 1) + BM25 (tantivy.go),
│   │                          # FalkorDB (falkordb.go), Redis
│   │                          # semantic cache (redis_cache.go)
│   │                          # (Phase 3)
│   ├── shard/                 # Phase 5: shard manifest API
│   │                          # (handler.go, repository.go),
│   │                          # generation worker (generator.go),
│   │                          # delta protocol (delta.go),
│   │                          # cryptographic-forgetting orchestrator
│   │                          # (forget.go), client contract
│   │                          # (contract.go) shared by iOS / Android /
│   │                          # desktop on-device runtimes, coverage
│   │                          # endpoint (handler.go::coverage),
│   │                          # GORM-backed CoverageRepo
│   │                          # (coverage_repo.go), version-lookup
│   │                          # adapter for the device-first hint
│   │                          # (version_lookup.go), and per-tier
│   │                          # eviction policy (eviction.go)
│   ├── models/                # Phase 5 / Task 13: model catalog.
│   │                          # ModelEntry / ModelCatalog / Provider /
│   │                          # StaticProvider (catalog.go) and
│   │                          # GET /v1/models/catalog (handler.go)
│   ├── b2c/                   # Phase 6 / Tasks 14 + 17: B2C client SDK
│   │                          # bootstrap. Mounts /v1/health,
│   │                          # /v1/capabilities, and
│   │                          # /v1/sync/schedule (handler.go)
│   ├── observability/         # Phase 8: OpenTelemetry tracing
│   │                          # helper + attribute-key constants
│   │                          # (tracing.go); used by the pipeline
│   │                          # coordinator and the retrieval
│   │                          # fan-out. Plus Prometheus collectors
│   │                          # (metrics.go) and a Gin middleware
│   │                          # (middleware.go) scraped at /metrics
│   │                          # on cmd/api and cmd/ingest. Phase 6 /
│   │                          # Phase 8 structured JSON logging:
│   │                          # logger.go wraps log/slog with a
│   │                          # tenant_id + trace_id JSON handler;
│   │                          # GinLoggerMiddleware injects the
│   │                          # request-scoped logger from the
│   │                          # W3C traceparent + tenant context
│   ├── grpcpool/              # Phase 8: round-robin gRPC connection
│   │                          # pool with per-call deadline +
│   │                          # circuit breaker (closed → open →
│   │                          # half-open) for the Python sidecars
│   ├── lifecycle/             # Phase 8 / Task 10: ordered shutdown
│   │                          # runner. Step.Add() / Run() with a
│   │                          # configurable deadline; used by
│   │                          # cmd/api and cmd/ingest to drain
│   │                          # in-flight work before closing
│   │                          # Postgres / Redis / Kafka.
│   ├── config/                # Phase 8 / Task 11: startup config
│   │                          # validation (validate.go). Aggregates
│   │                          # required-env + URL format errors
│   │                          # into a single ConfigError. ValidateAPI
│   │                          # / ValidateIngest run before any
│   │                          # gorm.Open / redis.NewClient call.
│   ├── migrate/               # Phase 8 / Task 18: SQL migration
│   │                          # runner (runner.go). Reads
│   │                          # migrations/NNN_name.sql in order,
│   │                          # tracks applied migrations in the
│   │                          # schema_migrations table, supports
│   │                          # DryRun. Wired behind AUTO_MIGRATE.
│   ├── eval/                  # Round-4 Task 1: retrieval evaluation
│   │                          # harness (suite + metrics + runner +
│   │                          # admin handler) — see §4.5
│   ├── errors/                # Round-4 Task 7: structured error
│   │                          # catalog (typed codes + Gin
│   │                          # middleware that maps Go errors to
│   │                          # the wire JSON shape)
│   ├── policy/                # see Phase 4 entry above; Phase 6 /
│   │                          # Task 15 added device_first.go
│   │                          # (Decide → prefer_local hint) consumed
│   │                          # by internal/retrieval/handler.go;
│   │                          # live_resolver.go now reads
│   │                          # channel_policies.deny_local_retrieval
│   │                          # so the channel_disallowed reason
│   │                          # round-trips end-to-end
│   ├── pipeline/              # see Phase 1 entry above; Phase 8 /
│   │                          # Task E15 added dlq_observer.go
│   │                          # (DLQ consumer → Prometheus counter
│   │                          # context_engine_dlq_messages_total +
│   │                          # structured per-envelope logging)
│   └── admin/                 # see Phase 2 / 4 entries. Phase 5 /
│                              # Task E16 added tenant_delete.go:
│                              # 5-step TenantDeleter workflow + the
│                              # DELETE /v1/admin/tenants/:tenant_id
│                              # endpoint backed by
│                              # migrations/008_tenant_status.sql.
│                              # Phase 8 / Task 5 added dlq_handler.go
│                              # (GET /v1/admin/dlq + replay), Task 7
│                              # added reindex_handler.go
│                              # (POST /v1/admin/reindex), Task 8 added
│                              # api_ratelimit.go (per-tenant Redis
│                              # token bucket on the /v1/ group),
│                              # Task 14 added sync_progress.go +
│                              # sync_progress_handler.go
│                              # (GET /v1/admin/sources/:id/progress),
│                              # Task 19 added dashboard_handler.go
│                              # (GET /v1/admin/dashboard).
├── proto/
│   ├── docling/v1/            # Python Docling parsing service
│   ├── embedding/v1/          # Python embedding service
│   ├── graphrag/v1/           # Round-4 Task 2: GraphRAG entity
│   │                          # extraction (ExtractEntities RPC,
│   │                          # nodes + edges, Stage 3b)
│   └── memory/v1/             # Mem0 persistent memory service
├── services/                  # Python ML microservices (Phase 3)
│   ├── _proto/                # generated Python gRPC stubs
│   ├── docling/               # Docling gRPC server + Dockerfile
│   ├── embedding/             # sentence-transformers gRPC server
│   ├── graphrag/              # Round-4 Task 2: GraphRAG gRPC server
│   │                          # (entity + relation extraction)
│   ├── memory/                # Mem0 gRPC server + Dockerfile
│   └── gen_protos.sh          # regenerates _proto/ from proto/
├── pkg/                       # public shared types (reserved)
├── migrations/
│   ├── 001_audit_log.sql      # audit_logs table + indexes
│   ├── 002_sources.sql        # Phase 2 sources table + indexes
│   ├── 003_source_health.sql  # Phase 2 source_health table
│   ├── 004_policy.sql         # Phase 4 tenant_policies +
│   │                          # channel_policies + policy_acl_rules
│   │                          # + recipient_policies tables
│   ├── 005_policy_drafts.sql  # Phase 4 policy_drafts table
│   │                          # (JSONB payload + status FSM)
│   ├── 006_shards.sql         # Phase 5 shards table (manifest +
│   │                          # version + chunk_count + status)
│   ├── 007_channel_deny_local.sql  # Phase 5 / 6: adds
│   │                          # channel_policies.deny_local_retrieval
│   │                          # — wires the channel_disallowed reason
│   │                          # in policy.device_first.go
│   ├── 008_tenant_status.sql  # Phase 5 / Task E16: tenants table
│   │                          # with tenant_status column for the
│   │                          # 5-step TenantDeleter workflow
│   ├── 009_dlq_messages.sql   # Phase 8 / Task 5: dlq_messages
│   │                          # table for the DLQ admin surface
│   ├── 010_retention_policy.sql # Phase 8 / Task 6: retention_rules
│   │                          # table backing the RetentionPolicy
│   │                          # layered tenant/source/namespace scope
│   ├── 011_varchar_ids.sql    # Round-4 Task 0: revert CHAR(N) to
│   │                          # VARCHAR(N) so wildcard sentinels
│   │                          # don't blank-pad on read
│   ├── 012_eval_suites.sql    # Round-4 Task 1: eval_suites table
│   │                          # for the retrieval evaluation harness
│   ├── 013_sync_schedules.sql # Round-4 Task 5: sync_schedules
│   │                          # table powering the cron scheduler
│   ├── 014_tenant_usage.sql   # Round-4 Task 17: tenant_usage
│   │                          # daily rollup table
│   └── rollback/              # Round-4 Task 20: per-migration
│                              # *.down.sql, applied via
│                              # `make migrate-rollback`
├── tests/
│   ├── e2e/                   # docker-compose smoke test
│   │                          # (build tag: //go:build e2e)
│   ├── integration/           # Go ↔ Python gRPC integration tests
│   │                          # (build tag: //go:build integration)
│   ├── benchmark/             # pipeline + retrieval benchmarks
│   ├── regression/            # Round-4 Task 15: PR #12 regression
│   │                          # manifest + meta-tests
│   └── capacity/              # Phase 8 capacity test
│                              # (`make capacity-test`,
│                              # CAPACITY_DOCS_PER_MIN +
│                              # CAPACITY_DURATION env tunables)
├── docs/                      # PROPOSAL / ARCHITECTURE / PHASES /
│   │                          # PROGRESS / CUTOVER
│   ├── runbooks/              # Phase 7 per-connector ops runbooks
│   │                          # (one Markdown file per connector +
│   │                          # a README index): credential rotation,
│   │                          # quota / rate-limit incidents, outage
│   │                          # detection, and connector-specific
│   │                          # error codes
│   └── contracts/             # Phase 5 / 6 client-side contracts:
│                              # uniffi-ios.md, uniffi-android.md,
│                              # napi-desktop.md,
│                              # local-first-retrieval.md,
│                              # bonsai-integration.md,
│                              # b2c-retrieval-sdk.md,
│                              # privacy-strip-render.md,
│                              # background-sync.md
├── deploy/                    # Phase 8 HorizontalPodAutoscaler
│                              # manifests: hpa-api.yaml,
│                              # hpa-ingest.yaml, hpa-docling.yaml,
│                              # hpa-embedding.yaml — each targets the
│                              # CPU + custom Prometheus metric for
│                              # its deployment
├── docker-compose.yml         # local dev: Postgres / Redis / Kafka /
│                              # Qdrant / FalkorDB / Docling /
│                              # embedding / memory
├── Makefile                   # build / test / vet / lint / proto-gen /
│                              # test-e2e / test-integration / bench
├── .github/workflows/ci.yml   # CI: vet / test / lint / proto-gen /
│                              # e2e / services-unit / integration
├── go.mod
└── go.sum
```

### Tech choices realised in Phase 0

- **Web framework:** `github.com/gin-gonic/gin` (matches `ai-agent-platform`).
- **ORM:** `gorm.io/gorm` with `gorm.io/driver/postgres` in production
  and `github.com/glebarez/sqlite` in repository tests.
- **Kafka client:** `github.com/IBM/sarama` (the user-stated default in the
  Phase 0 scope).
- **gRPC:** `google.golang.org/grpc` and `google.golang.org/protobuf`.
- **ULIDs:** `github.com/oklog/ulid/v2`.
- **Crypto:** `crypto/aes` + `crypto/cipher` from the standard library;
  the envelope format mirrors the wire format used by
  `ai-platform-backend-go/pkg/crypto/encryption` so payloads stay
  cross-compatible.
- **Observability:** `go.opentelemetry.io/otel` is wired through go.mod
  but not yet instrumented; instrumentation lands in Phase 1 alongside
  the pipeline.

### Tech choices added in Phase 1

- **Vector store client:** stdlib `net/http` + JSON against the Qdrant
  REST API (per-tenant collections, hard tenant filter on every search).
  A native Go client lands when Phase 3's fan-out work begins.
- **HTTP and external API clients (Google Drive, Slack):** stdlib
  `net/http` only — keeps the binary surface small and means the same
  test machinery (`httptest.Server`) covers every connector.
- **Postgres driver in production:** `gorm.io/driver/postgres`.
- **gRPC clients:** `google.golang.org/grpc` `NewClient` with
  `insecure.NewCredentials()` for in-cluster traffic; the pipeline
  embedder also exposes a `RemoteEmbedder` interface so external
  embedding APIs can plug in behind per-tenant policy.

### Tech choices added in Phase 3

- **BM25 search:** `github.com/blevesearch/bleve/v2` — pure-Go full
  text index. We chose `bleve` over `tantivy-go` because the latter
  requires a Rust toolchain at build time, which violates the "Go
  binary, no native deps" invariant. The BM25 client is wrapped
  behind a small interface so the backend can swap to `tantivy-go`
  later without changing the retrieval handler.
- **Graph traversal:** `github.com/FalkorDB/falkordb-go` (FalkorDB is
  a Redis module that speaks GRAPH.* commands). One graph per
  tenant, named after the tenant id.
- **Redis client / semantic cache:** `github.com/redis/go-redis/v9`.
  Cache key is a SHA-256 over `(tenant_id, channel_id,
  query_embedding, scope_hash)` with a per-tenant key prefix; tests
  use `github.com/alicebob/miniredis/v2` for an in-process Redis.
- **Errgroup fan-out:** `golang.org/x/sync/errgroup` with a per-call
  context derived from a per-backend deadline; backend errors are
  logged and surfaced as `policy.degraded`, never as a 5xx.
- **Python ML services:** `grpcio` + `grpcio-tools` for the gRPC
  server, `docling` for parsing, `sentence-transformers` for
  embeddings, and `mem0ai` for memory. Each service is a thin gRPC
  shim over the upstream library.

### Tech choices added in Phase 2

- **Source-management API:** Gin handlers in `internal/admin/`
  scoped under the auth middleware group. The `SourceRepository`
  uses GORM against Postgres in production and the in-memory
  `glebarez/sqlite` driver in tests.
- **Per-tenant Kafka routing:** Sarama `SyncProducer` with
  partition keys of the form `tenant_id||source_id` (the doubled
  `||` separator is the on-wire spec; `pipeline.PartitionKey`
  exposes a constant). The consumer parses keys with
  `pipeline.ParsePartitionKey` and rejects body/key mismatches as
  poison messages, defending against spoofed partition routing.
  Single-pipe `tenant_id|source_id` keys are accepted as a one-
  release migration aid and surfaced via
  `ConsumerConfig.OnLegacyKey` so production can metric the rate
  and remove the fallback once the topic drains.
- **Backfill rate control:** A `pipeline.RateController` interface
  decouples the orchestrator from the limiter implementation.
  `pipeline.TickerRate` is the wall-clock pacer used in tests; the
  production wiring uses `admin.BoundController` over the Redis
  token bucket.
- **Per-source rate limiter:** Atomic Lua token bucket in Redis
  keyed by `(tenant_id, source_id)`. Loaded once via `ScriptLoad`
  with a `Eval` fallback for `NOSCRIPT` after a Redis flush.
- **Source-health tracking:** A separate `source_health` Postgres
  table (one row per (tenant_id, source_id)) keeps `last_success_at`,
  `last_failure_at`, `lag`, `error_count`, and a derived `status`
  column. `admin.DeriveStatus` computes the status from configurable
  thresholds.
- **Forget-on-removal worker:** A Redis `SET NX EX` fenced lease
  prevents concurrent forget-job runs from racing a re-add. The
  worker fans out to a list of `ForgetSweeper` implementations (one
  per storage tier) so per-backend cleanup logic stays in the
  storage layer.

### Tech choices added in Phase 4

- **Privacy modes:** `internal/policy/privacy_mode.go` defines a
  total order over `no-ai` < `local-only` < `local-api` <
  `hybrid` < `remote`. `EffectiveMode` returns the stricter of
  (tenantMode, channelMode); unknown modes fail closed (treated
  as more strict than the strictest known mode).
- **Allow / deny ACL evaluator:** `internal/policy/acl.go` evaluates
  rules with deny-over-allow precedence. Rules can match by
  source ID, namespace ID, or path glob (with a `**`
  cross-segment extension on top of `path.Match`).
- **Recipient policy:** Per-(tenant, channel) allow/deny list of
  downstream skill consumers. `RecipientPolicy.IsAllowed` defaults
  open or closed according to the channel's
  `recipient_default` column.
- **Retrieval wiring:** `internal/retrieval/policy_snapshot.go`
  defines a `PolicyResolver` port and `PolicySnapshot` carrier;
  the handler resolves the snapshot before fan-out (failing
  closed on error) and applies ACL + recipient gates after the
  existing `PolicyFilter` runs.

### Tech choices added in Phase 4 (simulator)

- **Snapshot package home:** `internal/policy/snapshot.go` is the
  canonical home of `PolicySnapshot` and the `PolicyResolver`
  port; `internal/retrieval/policy_snapshot.go` keeps a type
  alias so the retrieval handler does not have to import the
  policy package's transitive deps. The snapshot exposes
  `Clone()` so the simulator can run draft policy through the
  retrieval pipeline without aliasing the live cache.
- **What-if simulator:** `internal/policy/simulator.go` runs
  retrieval twice — once with the live `PolicySnapshot` and once
  with the draft — through the same `RetrieverFunc`, then diffs
  the two hit lists into `Added` / `Removed` / `Changed`
  (privacy-label flips). The `Retriever` is a narrow port so
  tests can plug in a fake corpus and the production wiring can
  later forward to the real retrieval handler.
- **Data-flow diff:** `internal/policy/simulator_diff.go`
  aggregates hits by `privacy_label` to compute the per-tier
  count delta and a human-readable summary
  ("draft routes 12% more matches through 'remote'"). The
  summary's tie-breaker prefers tiers where `Live > 0` so
  brand-new buckets do not collapse the percentage to a raw
  count and hide the policy intent.
- **Conflict detection:** `internal/policy/simulator_conflict.go`
  surfaces three categories before promotion:
  `privacy_mode_override` (channel weaker than tenant),
  `acl_overlap` (deny+allow on the same path glob), and
  `recipient_contradiction` (channel allows a skill the tenant
  denies). Conflicts have `error` or `warning` severity and a
  deterministic order so admin-portal renders are stable across
  reads.
- **Draft policy store:** `internal/policy/draft.go` (table
  `policy_drafts`, migration
  `migrations/005_policy_drafts.sql`) is a per-tenant repository
  with a `draft → promoted | rejected` status FSM. Drafts are
  isolated from the live `PolicyResolver` — the resolver never
  reads from the drafts table — so an in-progress edit cannot
  leak into retrieval. `Get` / `MarkPromoted` / `MarkRejected`
  scope by `tenant_id` and return `ErrDraftNotFound` on a tenant
  mismatch, denying cross-tenant access without leaking the row's
  existence.
- **Audited promotion:** `internal/policy/promotion.go` is the
  promotion FSM. `PromoteDraft` runs conflict detection
  (blocks on any error-severity conflict), applies the snapshot
  to the live tables via the `LiveStore` port, marks the draft
  promoted, and emits a `policy.promoted` audit event — all in
  the same `*gorm.DB` transaction so partial failure leaves the
  draft in `draft` and the live tables untouched. `RejectDraft`
  is the rejection counterpart.
- **GORM live store:** `internal/policy/live_store.go` is the
  production `LiveStore`. It writes the live policy tables in
  `migrations/004_policy.sql` (`tenant_policies`,
  `channel_policies`, `policy_acl_rules`, `recipient_policies`)
  inside the supplied transaction. Wipe-and-replace semantics on
  the rule tables make a draft the desired-state representation:
  promote = "make live look exactly like this draft", with no
  diff-and-merge ambiguity. The audit emit is part of the same
  transaction: `policy.AuditWriter.CreateInTx` joins the
  `policy.promoted` / `policy.rejected` row to the outer tx, so a
  `LiveStore` failure rolls the audit log back along with the rest
  rather than leaving a "promoted" event for a promotion that never
  happened.
- **GORM live resolver:** `internal/policy/live_resolver.go`
  (`LiveResolverGORM`) is the read counterpart to `LiveStoreGORM`.
  It hydrates a `PolicySnapshot` from the same live policy tables —
  resolving the strict-vs-permissive merge of tenant + channel
  privacy mode, unioning tenant-wide and channel-specific ACL rules,
  and respecting the channel's `recipient_default` for
  `RecipientPolicy.DefaultAllow`. Wired into the retrieval
  handler's `PolicyResolver` port and into the simulator's
  `LiveResolver` port in `cmd/api/main.go`, so the same live state
  drives production retrieval and what-if simulation.
- **Snapshot-driven retrieval:**
  `retrieval.Handler.RetrieveWithSnapshot` is the simulator's
  in-process bridge into the retrieval pipeline. It runs the full
  embed → fan-out → RRF merge → rerank → ACL/recipient gate
  sequence against an explicit `PolicySnapshot`, deliberately
  bypassing the semantic cache (a draft snapshot's rules must not
  contaminate the live cache or vice versa). `cmd/api/main.go`
  projects `policy.SimRetrieveRequest` ↔ `retrieval.RetrieveRequest`
  through a thin `simulatorRetriever` adapter so the simulator and
  the gin handler share one retrieval implementation.
- **Admin HTTP surface:** `internal/admin/simulator_handler.go`
  mounts the policy endpoints under `/v1/admin/policy/`:
  `POST/GET /drafts`, `GET /drafts/:id`,
  `POST /drafts/:id/promote|reject`, `POST /simulate`,
  `POST /simulate/diff`, `POST /conflicts`. The handler is
  written against narrow ports (`DraftStore`, `PromotionService`,
  `SimulatorEngine`) so tests can run with in-memory fakes.
- **Privacy strip enrichment:** `internal/retrieval/privacy_strip.go`
  builds a structured `PrivacyStrip` (`mode`, `processed_where`,
  `model_tier`, `data_sources`, `policy_applied`) from the
  resolved `PolicySnapshot` and the chunk's `privacy_label`. The
  retrieval handler attaches the strip to every `RetrieveHit`
  on both the fresh and cached paths; the strip is rebuilt at
  serve time from the live snapshot rather than cached, so a
  policy change between cache write and read does not surface a
  stale disclosure.

### Tech choices added in Phase 5

- **Shard manifest API:** `internal/shard/handler.go` mounts
  `GET /v1/shards/:tenant_id` (full manifest list) and
  `GET /v1/shards/:tenant_id/delta?since=<v>` (incremental delta)
  against `internal/shard/repository.go`, a GORM-backed metadata
  store. The repository columns mirror the on-device shard contract
  (`tenant_id`, `user_id`, `channel_id`, `privacy_mode`,
  `shard_version`, `chunks_count`, `status`,
  `created_at`/`updated_at`) so a client can resume sync after a
  network interruption from the last seen `shard_version`.
- **Shard generation worker:** `internal/shard/generator.go` runs
  inside `cmd/ingest/main.go` as an optional Stage 4 fan-out hook
  triggered by a `shard.requested` Kafka event. It calls
  `policy.PolicyResolver.Resolve` for the requested
  (tenant, user, channel, privacy_mode) tuple before reading from
  the storage plane, so the produced shard is policy-bounded by
  construction — privacy-mode + ACL + recipient gates run before
  the chunk IDs and embeddings hit the manifest.
- **Delta sync protocol:** `internal/shard/delta.go` diffs two
  shard versions into add/remove operations and emits stable JSON
  the client can apply offline. Versions are monotonic per
  (tenant_id, user_id, channel_id, privacy_mode) so concurrent
  generations on the server don't observe ABA.
- **Cryptographic-forgetting orchestrator:**
  `internal/shard/forget.go` extends the Phase 2
  `internal/admin/forget_worker.go` pattern to the full tenant
  delete workflow described in §5: mark `pending_deletion` →
  drain the pipeline → drop Qdrant collections, FalkorDB graphs,
  Tantivy directories, and Redis keys → destroy the per-tenant
  DEKs in the credential store → mark `deleted`.
  `cmd/api/main.go` mounts `DELETE /v1/tenants/:tenant_id/keys`
  as the trigger.

### Tech choices added in Phase 7

- **Connector skeleton:** Each new connector (`internal/connector/<name>/`)
  follows the Phase 1 pattern — stdlib `net/http`, a small
  `Connection` struct implementing `connector.Connection`, an
  iterator-style `DocumentIterator` for paginated listing, and an
  `init()` side-effect that calls
  `connector.RegisterSourceConnector`. Tests use
  `httptest.NewServer` against handcrafted JSON fixtures.
- **Microsoft Graph (SharePoint, OneDrive, Teams):** Bearer token
  auth on every request; delta tokens captured from the
  `@odata.deltaLink` query parameter; pagination follows
  `@odata.nextLink`. Teams encodes its hierarchical
  Team→Channel→Messages structure as `team_id/channel_id` in the
  `Namespace.ID` so the same iterator can serve list and fetch.
- **Atlassian (Confluence, Jira):** Basic auth with email + API
  token. Jira additionally implements `WebhookReceiver` for the
  Atlassian webhook payload (issue created / updated / deleted).
- **Dropbox / Box:** Bearer token auth; `list_folder/continue`
  cursor for Dropbox delta and the `events` stream
  (`next_stream_position`) for Box.
- **Notion:** Notion REST API with `last_edited_time` filter for
  delta and bearer-token auth.
- **GitHub / GitLab:** REST API with personal-access-token auth
  (`Authorization: token <pat>` for GitHub, `PRIVATE-TOKEN: <pat>`
  for GitLab). Both implement `WebhookReceiver` for issue events.
- **Connector registration wiring:** `cmd/api/main.go` and
  `cmd/ingest/main.go` blank-import every connector so the
  `init()` registry hooks fire on startup. The order is
  alphabetical to keep diffs minimal across phases.

### Tech choices added in Phase 8

- **OpenTelemetry tracing (`internal/observability/`):** A small
  package centralises the tracer name (`context-engine`), the
  `StartSpan(ctx, name, attrs...)` helper, the
  `RecordError(span, err)` shim, and the attribute keys reused
  across packages (`tenant_id`, `document_id`, `backend`,
  `hit_count`, `latency_ms`, `stage`). The pipeline coordinator
  wraps each retry of each stage in `pipeline.<stage>` and the
  retrieval fan-out wraps each backend call in
  `retrieval.<backend>` with `latency_ms` + `hit_count`
  attributes. `RetrieveResponse.TraceID` echoes the trace id so
  clients can correlate slow requests with backend traces.
- **Per-stage worker pools (`pipeline.StageConfig`):** The
  coordinator spawns N goroutines per stage that share the
  upstream channel, with the downstream channel closed via a
  `sync.WaitGroup` after all stage workers exit — so adding
  parallelism does not race the close-on-shutdown invariant.
  Defaults are 1-per-stage to preserve pre-Phase-8 ordering;
  callers crank up `EmbedWorkers` first since the embedder is
  normally the long-pole.
- **Sticky Kafka rebalance (`pipeline.ConsumerTuning` +
  `SaramaConfigWith`):** Sticky assignment keeps a partition glued
  to one consumer across restarts, preserving per-source ordering
  through a rebalance. `SessionTimeout` and `MaxPollInterval` are
  exposed for tuning against the upstream broker config.
- **Storage connection pools:** Qdrant uses a sized
  `http.Transport` (`MaxIdleConnsPerHost` configurable via
  `CONTEXT_ENGINE_QDRANT_POOL_SIZE`); the Redis client used by the
  semantic cache and the FalkorDB adapter takes
  `CONTEXT_ENGINE_REDIS_POOL_SIZE`; Postgres goes through GORM's
  `*sql.DB` with `SetMaxOpenConns` / `SetMaxIdleConns` /
  `SetConnMaxLifetime` driven by `CONTEXT_ENGINE_PG_MAX_OPEN` and
  `CONTEXT_ENGINE_PG_MAX_IDLE`.
- **gRPC connection pool (`internal/grpcpool/`):** Round-robin
  selection across `Size` long-lived `*grpc.ClientConn`s with a
  per-call deadline and a circuit breaker. The breaker tracks
  consecutive failures; once `Threshold` is hit it transitions to
  open, refuses calls for `OpenFor`, then half-opens — a single
  trial call decides closed (success) or open (failure). Callers
  Borrow / Release; the helper does not wrap the generated stubs
  because every sidecar has a different proto surface.
- **Capacity test harness (`tests/capacity/`):** A Go test that
  submits N documents/minute through the coordinator with fake
  fetch / parse / embed / store stages, asserts every Submit
  completes within a per-call deadline (no producer
  back-pressure), and logs P50 / P95 / P99 submit latency.
  `make capacity-test` runs it in the standard test profile;
  `CAPACITY_DOCS_PER_MIN` and `CAPACITY_DURATION` configure the
  load shape for soak runs.
- **Prometheus metrics (`internal/observability/metrics.go` +
  `middleware.go`):** Six Go collectors register against a shared
  `prometheus.Registry` (so unit tests can spin up an isolated
  registry per test): `context_engine_api_requests_total`,
  `context_engine_api_request_duration_seconds`,
  `context_engine_kafka_consumer_lag`,
  `context_engine_pipeline_stage_duration_seconds`,
  `context_engine_retrieval_backend_duration_seconds`,
  `context_engine_retrieval_backend_hits`. The Gin middleware
  records request count + duration on every API route. The
  pipeline coordinator records per-stage duration via
  `observability.ObserveStageDuration` from
  `runWithRetry`. The retrieval handler records per-backend
  duration via `observability.ObserveBackendDuration` from
  `fanOut`. The Kafka consumer reports lag via
  `observability.SetKafkaConsumerLag` after every commit. The
  Python sidecars share `services/_metrics.py::ServiceMetrics`,
  which exposes a per-prefix triplet (`<prefix>_requests_total`,
  `<prefix>_duration_seconds`, `<prefix>_queue_depth`) on a
  separate sidecar HTTP listener (default port 9090).
- **HPA manifests (`deploy/`):** Four Kubernetes HPAs target the
  matching collectors: `hpa-api.yaml` scales the API deployment
  on CPU + `context_engine_api_requests_per_second`;
  `hpa-ingest.yaml` scales the ingest deployment on CPU +
  `context_engine_kafka_consumer_lag`; `hpa-docling.yaml` and
  `hpa-embedding.yaml` scale the Python sidecars on CPU +
  memory + `<prefix>_queue_depth`. Each manifest sets explicit
  scale-up / scale-down stabilization windows so the autoscaler
  is not chatty under bursty traffic.
- **Mem0 tenant prefix partitioning
  (`services/memory/memory_server.py`):** Every Mem0 `add` /
  `search` keys on `<tenant_prefix>:<user_id>`, where the prefix
  is resolved from `MEM0_TENANT_PREFIX_TEMPLATE` (default
  `"{tenant_id}"`). Metadata records both `tenant_id` and
  `tenant_prefix`; `search` additionally drops stray rows whose
  metadata `tenant_id` mismatches as a defence-in-depth guard.
  The Go memory client (`cmd/api/main.go::memoryAdapter`) passes
  `tenant_id` on every `SearchMemory` gRPC call.
- **Liveness / readiness probes (`cmd/api/readyz.go`,
  `cmd/ingest/health.go`):** Both binaries expose `GET /healthz`
  (always 200) and `GET /readyz` (returns 503 + the failed
  dependency name). The API binary checks Postgres + Redis +
  Qdrant; the ingest binary checks Postgres + Redis + every
  Kafka broker via `net.DialTimeout`. The probes share the same
  HTTP listener as `/metrics`, so a single Kubernetes
  `livenessProbe` / `readinessProbe` selector covers both.
- **Retrieval latency optimisations:** `QdrantClient.Warmup` issues
  N parallel `GET /` requests on startup to pre-establish the
  `http.Transport` connection pool; `FalkorDBClient.KeepAlive`
  runs a background ticker that pings `GRAPH.LIST` so the redis
  connection pool stays warm during idle periods. Both are
  wired into `cmd/api/main.go` after the listener starts. A new
  `RetrieveResponse.Timings` envelope (vector_ms / bm25_ms /
  graph_ms / memory_ms / merge_ms / rerank_ms) breaks each
  request's wall-clock down by backend so operators can
  identify the long pole without reaching for traces. The
  budget is enforced by
  `tests/benchmark/p95_e2e_test.go::TestE2E_RetrieveP95` (build
  tag `e2e`, `make bench-e2e`) which fails the build if
  retrieval P95 > 500 ms or round-trip P95 > 1 s.

### Tech choices added in Phase 6

- **B2C client SDK bootstrap (`internal/b2c/`):** A single Gin
  handler mounts three endpoints the B2C splash / boot path
  consumes: `GET /v1/health` returns `status` + server time +
  build version (cheap, no DB); `GET /v1/capabilities` reports
  enabled retrieval backends + supported privacy modes + the
  `device_first` / `local_shard_sync` feature flags so a B2C UI
  built three months ago can still degrade gracefully against a
  newer server; `GET /v1/sync/schedule` returns recommended
  foreground / background polling intervals (defaults: 60 s
  foreground, 15 min background, ±30 s jitter, 30 s / 5 min
  hard floors). The wire format is contract-pinned in
  `docs/contracts/b2c-retrieval-sdk.md` and
  `docs/contracts/background-sync.md`.
- **Device-first policy (`internal/policy/device_first.go`):**
  `Decide(DeviceFirstInputs) DeviceFirstDecision` is a pure
  function that returns a structured `prefer_local` hint with a
  stable failure label (`device_tier_too_low`,
  `channel_disallowed`, `privacy_blocks_local`,
  `no_local_shard`, `prefer_local`). The retrieval handler
  consults it on every successful `POST /v1/retrieve` (cache
  hit + fresh fan-out) and surfaces the result on the response
  envelope (`RetrieveResponse.prefer_local`,
  `.local_shard_version`, `.prefer_local_reason`).
  `shard.VersionLookup` adapts the GORM-backed shard repository
  to the retrieval handler's narrow `ShardVersionLookup` port —
  shards live in a separate package from retrieval to avoid an
  import cycle, and lookup errors fail closed to
  `prefer_local=false` so a transient DB hiccup degrades
  gracefully.

### Tech choices added in Phase 5 (Tasks 9-13, 19)

- **Client contract interface (`internal/shard/contract.go`):**
  `ShardClientContract` is the four-method contract every
  on-device runtime (iOS XCFramework, Android AAR, Electron
  N-API addon) must implement. The Go-side interface lives in
  the shard package and is mirrored in
  `docs/contracts/uniffi-ios.md`,
  `docs/contracts/uniffi-android.md`, and
  `docs/contracts/napi-desktop.md`. The supporting wire types
  (`ShardScope`, `ShardDelta`, `LocalQuery`,
  `LocalRetrievalResult`, `LocalHit`) carry JSON tags that match
  the on-device runtime's serialization so the same struct
  round-trips between Go and Rust without translation.
- **Shard coverage endpoint
  (`internal/shard/handler.go::coverage`):** `GET /v1/shards/`
  `:tenant_id/coverage?privacy_mode=...` returns
  `CoverageResponse` (shard chunks, corpus chunks,
  coverage_ratio, is_authoritative). Clients use it to run the
  decision tree in `docs/contracts/local-first-retrieval.md`.
  The `CoverageRepo` port is optional; when no implementation
  is wired, the handler returns `is_authoritative=false` so the
  client treats the ratio as advisory and falls back to remote.
- **Model catalog (`internal/models/`):** `ModelCatalog`,
  `ModelEntry`, and `Provider` give the API binary a typed
  surface for the on-device model manifest;
  `StaticProvider` ships a hard-coded baseline of three
  Bonsai-1.7B builds (q4_0 / q8_0 / fp16) with the per-tier
  `EvictionPolicy` baked into the response. `Validate()` runs
  on construction so a typo in a hard-coded entry can't ship.
  `EligibleForTier(tier)` returns the smallest model whose
  `tier_floor` ≤ the device's reported tier.
  `GET /v1/models/catalog` is the wire endpoint; the contract
  is documented in `docs/contracts/bonsai-integration.md`.
- **Eviction policy (`internal/shard/eviction.go`):**
  `EvictionPolicy` carries `MaxShardSizeMB`,
  `MinFreeMemoryMB`, and `ThermalEvictMultiplier`;
  `ShouldEvict(EvictionInputs) EvictionDecision` is a pure
  function returning a stable reason
  (`unknown_tier` / `shard_too_large` / `memory_pressure` / `keep`).
  `DefaultEvictionPolicies()` ships baseline policies for Low /
  Mid / High; the policies are part of the catalog response so
  they can be tuned server-side without an on-device release.

### Tech choices added in Phase 8 (Bonsai contract)

- **Bonsai benchmark contract
  (`tests/benchmark/bonsai_contract_test.go`):**
  `BonsaiContract` is a Go-side slice of `TierBenchmark` rows
  defining min tokens/sec, max first-token latency, and max
  memory per tier. `SatisfiesContract(tier, tps, ttft, mem)`
  is the helper the on-device repos (`kennguy3n/knowledge`,
  `kennguy3n/llama.cpp`) call against measured numbers; an
  unknown tier or any out-of-envelope tuple returns a stable
  failure label so on-device CI can fail fast with a clear
  diagnostic.

### Tech choices added in Round 6

Round 6 adds breadth across retrieval quality, multi-tenant
operability, and pipeline observability. Each entry below is
self-contained and references the package + admin surface that
implements it.

- **MMR diversifier** (`internal/retrieval/diversifier.go`).
  Reranker-adjacent stage that re-orders the merged result set
  with `(1-lambda) * relevance - lambda * max_similarity` — i.e.
  higher `lambda` ⇒ more diversification, matching the API
  contract on `RetrieveRequest.Diversity`. Lambda defaults to 0.0
  (passthrough, pure relevance) so existing deployments are
  unaffected; clients opt in via `RetrieveRequest.Diversity`.
- **Semantic deduplication** (`internal/pipeline/dedup.go`).
  Stage-4-adjacent pre-write hook computing cosine similarity
  between chunk embeddings. Drops near-duplicates above a
  configurable threshold; toggled via `CONTEXT_ENGINE_DEDUP_ENABLED`
  (threshold via `CONTEXT_ENGINE_DEDUP_THRESHOLD`, default 0.95).
- **Per-source embedding model config**
  (`internal/admin/embedding_config.go`,
  `migrations/019_source_embedding_config.sql`). Tenants set a
  non-default model on a (tenant, source) basis; the embed stage
  reads the config and forwards the model name to the embedding
  gRPC service.
- **Query expansion via synonyms**
  (`internal/retrieval/query_expander.go`,
  `internal/admin/synonyms_handler.go`). A `SynonymExpander`
  augments the inbound query before fan-out, expanding the text
  sent to BM25 and memory backends without changing the vector
  query (vectors already capture synonymy).
- **Priority queues in pipeline coordinator**
  (`internal/pipeline/priority.go`,
  `internal/pipeline/coordinator.go`). High-priority items
  (steady-state syncs, admin overrides) cut to the front of the
  Stage-1 queue; low-priority items (backfills) age in and are
  not starved beyond a configurable timeout.
- **Chunk-level ACL** (`internal/policy/chunk_acl.go`,
  `migrations/020_chunk_acl.sql`). Per-chunk allow/deny rules
  evaluated after the source-level AllowDenyList; useful for
  cases where one chunk inside an allowed namespace must be
  hidden (PII, legal hold, etc.).
- **Adaptive connector rate limiting**
  (`internal/connector/adaptive_rate.go`). Wraps the existing
  Redis token bucket; on a 429 the bucket rate halves (down to a
  floor); on success the rate creeps back up. Prevents thundering
  herds against rate-limited SaaS APIs.
- **Source schema discovery endpoint**
  (`internal/admin/schema_handler.go`). Calls the connector's
  `ListNamespaces` so operators can preview what a source exposes
  before wiring policy.
- **Pipeline backpressure metrics**
  (`internal/observability/metrics.go`,
  `deploy/alerts/pipeline_backpressure.yaml`). Gauge
  `context_engine_pipeline_channel_depth{stage}` is updated after
  every coordinator submit; the alert rule fires when any
  channel sits above the warning threshold for 5 minutes.
- **Retrieval A/B testing framework**
  (`internal/admin/abtest.go`, `migrations/021_ab_tests.sql`).
  Active experiments route a deterministic percentage of
  requests (FNV bucket of the request key) to a `variant_config`
  and log both arms for offline comparison.
- **Admin notification preferences**
  (`internal/admin/notification.go`,
  `migrations/022_notification_preferences.sql`). Per-tenant
  webhook/email subscriptions fan audit events (sync started,
  source purged, policy promoted) to operator-owned endpoints.
- **Shard pre-generation scheduler**
  (`internal/shard/scheduler.go`). Background loop that
  enumerates active tenants and emits `shard.requested` events
  for stale or missing shards so the hot read path never waits
  on a lazy build.
- **API versioning middleware**
  (`internal/observability/api_version.go`). Resolves the
  client's chosen API version from `/vN/` path prefix or the
  `Accept-Version` header, rejects unknown versions with 406,
  echoes the resolved version in `X-API-Version`.
- **Tenant data portability**
  (`internal/admin/tenant_export.go`). Asynchronous full-tenant
  export job; collector + publisher are pluggable so production
  can stream to S3 / Postgres LO without code changes.
- **Pipeline dry-run mode**
  (`internal/pipeline/reindex.go`,
  `internal/admin/reindex_handler.go`). Operators can preview
  the cardinality of a reindex submission before unleashing it
  by passing `dry_run: true`; the orchestrator skips emission
  and returns the enumerated document count.
- **Connector template system**
  (`internal/admin/connector_template.go`,
  `migrations/023_connector_templates.sql`). Operators codify
  per-tenant connector defaults; `POST /v1/admin/sources`
  accepts an optional `template_id` and merges the template's
  `default_config` into the new source.
- **Cross-tenant isolation audit**
  (`internal/admin/isolation_audit.go`). Pluggable per-backend
  checkers (Qdrant collections, Redis prefixes, FalkorDB graphs)
  produce a structured pass/fail report; useful as a scheduled
  smoke test.
- **SSE streaming retrieval**
  (`internal/retrieval/stream_handler.go`). `POST
  /v1/retrieve/stream` emits `event: backend` frames as each
  backend completes plus a terminal `event: done` carrying the
  policy-filtered merged result; clients render partial UI as
  vector / BM25 / graph / memory finish.
- **Pipeline stage retry analytics**
  (`internal/pipeline/retry_analytics.go`,
  `internal/admin/retry_stats_handler.go`). Each stage records
  retry/success/failure outcomes and per-reason counts;
  `GET /v1/admin/pipeline/retry-stats` exposes a JSON snapshot
  used by dashboards and runbook automation.

### Tech choices added in Round 7

Round 7 layers operational hardening on top of Round 6: every
new feature reuses an existing storage/admin surface and most
hang off the same Postgres migrations chain (024 → 031). The
import graph stays one-way (admin imports retrieval; retrieval
never imports admin) thanks to a pair of new setters on
`*retrieval.Handler` that let the wiring layer attach the
admin-owned ABTestRouter and QueryAnalyticsRecorder after
construction.

- **Retrieval query analytics**
  (`internal/admin/query_analytics.go`,
  `migrations/024_query_analytics.sql`). Every successful
  Retrieve emits a `QueryAnalyticsEvent` containing the truncated
  query text, query hash, top-k, per-backend timings, hit count,
  cache-hit flag, and (when set) experiment name/arm.
  `GET /v1/admin/analytics/queries` supports time-range,
  tenant, and top-N filters; the top-N path feeds the cache
  warmer (Task 9).
- **Notification dispatcher retry + dead-letter**
  (`internal/admin/notification.go`,
  `internal/admin/notification_delivery_log.go`,
  `migrations/025_notification_delivery_log.sql`).
  `WebhookDelivery.Send` now retries with 1s/5s/15s
  exponential backoff; every attempt is persisted to
  `notification_delivery_log` and surfaced via
  `GET /v1/admin/notifications/delivery-log`.
- **A/B test results aggregator**
  (`internal/admin/abtest_results.go`). Reads `query_analytics`
  rows tagged with an experiment + arm and groups them per arm
  to compute avg/p50/p95 latency, hit count, and cache-hit rate.
  Endpoint: `GET /v1/admin/retrieval/experiments/:name/results`.
- **Credential health worker**
  (`internal/admin/credential_health.go`,
  `migrations/026_credential_valid.sql`). Periodically calls
  `connector.Validate()` for every active source, persists a
  boolean `credential_valid` on `source_health`, and emits a
  `source.credential_invalid` audit event on failure. Endpoint:
  `GET /v1/admin/sources/:id/credential-health`.
- **Retrieval cache warming**
  (`internal/retrieval/cache_warmer.go`,
  `internal/admin/cache_warm_handler.go`). Replays a list of
  `(tenant, query)` tuples through the retrieval handler to
  populate the Redis semantic cache; optional `auto_top_n` mode
  pulls the tuples from `query_analytics`. Endpoint:
  `POST /v1/admin/retrieval/warm-cache`.
- **Bulk source operations**
  (`internal/admin/bulk_source_handler.go`). Fan-out
  pause/resume/disconnect with per-source error isolation —
  one failure does not abort the batch. Each per-source action
  emits its own audit event. Endpoint:
  `POST /v1/admin/sources/bulk`.
- **Per-tenant latency budgets**
  (`internal/admin/latency_budget.go`,
  `migrations/027_latency_budgets.sql`). Stores per-tenant
  `max_latency_ms` and seeds the retrieval handler's request
  default when the request omits the field. Endpoints:
  `GET/PUT /v1/admin/tenants/:id/latency-budget`.
- **Chunk quality scoring**
  (`internal/pipeline/chunk_scorer.go`,
  `internal/admin/chunk_quality_handler.go`,
  `migrations/028_chunk_quality.sql`). Stage-4 pre-write hook
  scores each chunk on text length, language detection
  confidence, embedding magnitude, and duplicate ratio.
  Aggregated per source via
  `GET /v1/admin/chunks/quality-report`.
- **Source sync conflict resolver**
  (`internal/pipeline/conflict_resolver.go`). Last-writer-wins
  policy keyed on a monotonic `content_version` per
  (tenant, document); stale writes are dropped, emitting a
  `chunk.conflict_resolved` audit event with the resolution
  strategy.
- **Audit trail export**
  (`internal/admin/audit_export.go`). Streams matching audit
  rows as CSV or JSON Lines using chunked transfer encoding so
  multi-million-row exports finish without buffering. Endpoint:
  `GET /v1/admin/audit/export?format=csv|jsonl&since=&until=&...`.
- **Per-tenant cache TTL**
  (`internal/admin/cache_config.go`,
  `migrations/029_cache_config.sql`). Stores per-tenant
  semantic-cache TTL; the Redis writer falls back to the global
  default when no row exists. Endpoints:
  `GET/PUT /v1/admin/tenants/:id/cache-config`.
- **Connector sync history**
  (`internal/admin/sync_history.go`,
  `migrations/030_sync_history.sql`). Pipeline consumer records
  start/end/status/docs_processed/docs_failed per sync run.
  Endpoint: `GET /v1/admin/sources/:id/sync-history?limit=N`.
- **Retrieval result pinning**
  (`internal/admin/pinned_results.go`,
  `internal/retrieval/pin_apply.go`,
  `migrations/031_pinned_results.sql`). Admins pin specific
  chunk IDs to fixed positions for an exact-match query. The
  retrieval handler invokes `ApplyPins` after the policy filter
  and before MMR diversity, deduplicating any hit that already
  appears in the pin list. Endpoints:
  `POST/GET/DELETE /v1/admin/retrieval/pins`.
- **Pipeline stage health dashboard**
  (`internal/admin/pipeline_health.go`). Reads the in-process
  Prometheus registry and aggregates per-stage throughput,
  P50/P95 latency, retry rate, queue depth, and DLQ totals.
  Endpoint: `GET /v1/admin/pipeline/health`.
- **Rollback scripts 015–031**
  (`migrations/rollback/*.down.sql`). Every numeric prefix
  under `migrations/` now has a matching `down.sql`;
  `migrations/rollback/rollback_test.go` locks the contract.

---

## 10. Deployment & Scaling

### 10.1 Kubernetes deployments

Both Go binaries (`context-engine-api`, `context-engine-ingest`)
and the three Python sidecars (`docling`, `embedding`, `memory`)
are deployed as Kubernetes `Deployment`s with explicit
`resources.requests` and `resources.limits`. The HPA manifests
in [`deploy/`](../deploy/) target each deployment by name:

| Manifest | Target | Replicas (min/max) | Trigger metric |
|----------|--------|--------------------|----------------|
| `deploy/hpa-api.yaml` | `context-engine-api` | 2 / 20 | CPU 70% + `context_engine_api_requests_per_second` |
| `deploy/hpa-ingest.yaml` | `context-engine-ingest` | 2 / 16 | CPU 70% + `context_engine_kafka_consumer_lag` |
| `deploy/hpa-docling.yaml` | `docling` | 2 / 12 | CPU 80% + memory 80% + `docling_parse_queue_depth` |
| `deploy/hpa-embedding.yaml` | `embedding` | 2 / 16 | CPU 80% + memory 85% + `embedding_queue_depth` |

Each manifest sets explicit
`behavior.scaleUp.stabilizationWindowSeconds` (60 s) and
`behavior.scaleDown.stabilizationWindowSeconds` (300 s) so the
autoscaler does not thrash under bursty traffic. The custom
metrics are exposed on the deployment's `/metrics` endpoint and
scraped by the cluster's Prometheus adapter, which projects them
into the Kubernetes `external.metrics.k8s.io` / `pods/` API the
HPA consumes.

### 10.2 Resource sizing guidance

The defaults below are starting points; tune against measured
load.

| Workload | CPU request | CPU limit | Memory request | Memory limit | Notes |
|----------|-------------|-----------|----------------|--------------|-------|
| `context-engine-api` | 500m | 2 | 512 Mi | 1 Gi | Gin handlers + Qdrant / FalkorDB / Mem0 fan-out; the long pole is the embedder gRPC call so memory stays modest |
| `context-engine-ingest` | 500m | 2 | 512 Mi | 1 Gi | Kafka consumer + 4-stage pipeline; bump for high-fanout connectors |
| `docling` | 1 | 4 | 1 Gi | 4 Gi | Parsing is CPU-heavy; PDF / DOCX peaks dominate |
| `embedding` | 2 | 8 | 2 Gi | 8 Gi | sentence-transformers loads a 90 MB model; memory floor is 2 Gi |
| `memory` (Mem0) | 200m | 1 | 256 Mi | 1 Gi | Lightweight gRPC wrapper |

The Postgres pool size (`CONTEXT_ENGINE_PG_MAX_OPEN`) and the
Redis pool size (`CONTEXT_ENGINE_REDIS_POOL_SIZE`) should track
the API binary's CPU limit (one connection per ~125m of CPU
limit is a defensible starting point); the Qdrant pool
(`CONTEXT_ENGINE_QDRANT_POOL_SIZE`) tracks the expected QPS
divided by the average vector-search latency.

### 10.3 Probes and readiness

Both Go binaries expose `/healthz` (cheap liveness) and
`/readyz` (dependency-aware readiness) on the same listener as
`/metrics`. The API binary checks Postgres + Redis + Qdrant; the
ingest binary checks Postgres + Redis + every Kafka broker via
`net.DialTimeout`. A single Kubernetes `livenessProbe` /
`readinessProbe` selector covers both binaries.

### 10.4 Stage-aware concurrency

The ingest binary's per-stage worker pools
(`pipeline.StageConfig`) live alongside the deployment's CPU
request: the four `*Workers` env vars
(`CONTEXT_ENGINE_FETCH_WORKERS`,
`CONTEXT_ENGINE_PARSE_WORKERS`,
`CONTEXT_ENGINE_EMBED_WORKERS`,
`CONTEXT_ENGINE_STORE_WORKERS`) crank up parallelism on the
goroutine side without pushing the pod to a higher replica
count. The embedder is normally the long-pole, so
`EMBED_WORKERS` is the first knob to turn.

### 10.5 Rollout strategy

API binary deployments use `RollingUpdate` with `maxSurge: 1`
and `maxUnavailable: 0` so retrieval availability stays
constant during a release. Ingest deployments allow
`maxUnavailable: 1` since Kafka consumer rebalances are
sticky-tuned (`pipeline.ConsumerTuning`) and a brief partition
re-assignment is acceptable.

The Python sidecars use `RollingUpdate` with `maxSurge: 1` and
`maxUnavailable: 0` plus a 30 s `terminationGracePeriodSeconds`
so an in-flight gRPC call can drain.

### 10.6 On-device tier scaling

The on-device tier does not scale horizontally — each device
runs its own runtime. Scaling concerns instead surface as:

1. **Model catalog updates** — `GET /v1/models/catalog` ships a
   per-tier eviction policy (`eviction_config`) so server
   operators can tune shard retention without an on-device
   release. See `docs/contracts/bonsai-integration.md`.
2. **Sync schedule updates** — `GET /v1/sync/schedule` lets the
   server shorten polling during a heavy reindex (e.g. when a
   tenant adds a new connector with millions of documents) so
   the on-device shard catches up faster, then relax once the
   catch-up is done. See `docs/contracts/background-sync.md`.
3. **Benchmark contract** — `BonsaiContract` defines the
   per-tier performance floor each on-device measurement must
   clear; failing tiers stop publishing the affected platform
   release.

### 10.7 Alerting (Round-4 Task 10)

`deploy/alerts.yaml` is a Prometheus `PrometheusRule` manifest
that ships alongside the HPA manifests in `deploy/`. The rule
groups cover:

| Alert | Expression (paraphrased) | Severity |
|---|---|---|
| `IngestionLagHigh` | `context_engine_kafka_consumer_lag > 10000` for 5m | page |
| `DLQRateHigh` | `rate(context_engine_dlq_messages_total[5m]) > 1` | page |
| `RetrievalP95High` | retrieval handler P95 > 500ms for 5m | warn |
| `SourceUnhealthy` | per-source `error_count` over threshold | warn |
| `PipelineStageSlow` | per-stage P95 anomaly | warn |

`make alerts-check` runs
`internal/observability/alertcheck` against the YAML to validate
the rule shape (every group has a name, every rule has an
expression, severity is set). The alert names match the runbook
filenames under `docs/runbooks/` so an on-call engineer can hop
from PagerDuty to a runbook in a single click.

### 10.8 Health and operational endpoints (Round-4)

Three operational endpoints supplement Kubernetes liveness
probes:

- `GET /v1/admin/health/indexes` (Round-4 Task 19,
  `internal/admin/index_health.go`) runs every registered
  `BackendChecker` (postgres, qdrant, redis when configured)
  in parallel with a 5s timeout. Returns 200 with a
  per-backend status payload when every backend is green;
  returns 503 with the same payload when any backend fails so
  load balancers can drain unhealthy replicas without
  guessing.
- `GET /v1/admin/sources/:id/sync/stream` (Round-4 Task 18,
  `internal/admin/sync_progress_stream.go`) is a Server-Sent
  Events stream that polls `sync_progress` every
  `StreamPollInterval` (5s), diffs counters against the prior
  snapshot, and emits `discovered` / `processed` / `failed` /
  `completed` events plus a 30s heartbeat for proxy
  keepalive.
- `GET /v1/admin/tenants/:id/usage` (Round-4 Task 17,
  `internal/admin/metering.go`) returns a daily rollup of the
  tenant's API calls and ingestion counters from the
  `tenant_usage` table. The in-process `Counter`
  (`FlushOnInterval`) buffers per-request increments and
  flushes them via UPSERT, so the hot retrieval path never
  pays a per-request DB write.

### 10.9 Migration rollbacks (Round-4 Task 20)

Every forward migration in `migrations/` ships a matching
`migrations/rollback/<n>_<name>.down.sql`. `make migrate-rollback`
walks them in reverse against `CONTEXT_ENGINE_DATABASE_URL`. Each
rollback drops the indexes the forward migration created, then
the tables, using `IF EXISTS` for idempotency. Migrations that
ALTER COLUMN (`007_channel_deny_local`,
`011_varchar_ids`) revert their column shape changes. The
`migrations/rollback/rollback_test.go` test pins one rule:
**every numeric prefix under `migrations/` has a matching
prefix under `migrations/rollback/`** — a reviewer who forgets
the down-script gets caught at unit-test time, not at incident
time.

### Tech choices added in Round 9

- **GORM cutover complete.** The five remaining in-memory admin
  fakes (`NotificationStore`, `ABTestStore`,
  `ConnectorTemplateStore`, `SynonymStore`, `ChunkQualityStore`)
  are now Postgres-backed. Each follows the same Round-8 pattern:
  a `*Row` struct with `TableName()`, an `AutoMigrate` call inside
  `cmd/api/main.go`, and SQLite-backed unit tests using
  `github.com/glebarez/sqlite` so the package test suite stays
  hermetic. After Round 9, no admin handler has an in-memory store
  fallback — every persisted handler is Postgres-only.

- **Post-merge cross-backend dedup.** The RRF merger
  (`internal/retrieval/merger.go`) now collapses chunks with the
  same `chunk_id` arriving from multiple backends (vector + BM25 +
  graph) before they reach the reranker, keeping the higher-scored
  entry. This bounds the rerank request size to `O(distinct_chunks)`
  instead of `O(backends × top_k)`, which matters on the long tail
  of queries where ≥2 backends return the same blob.

- **Per-stage timeouts decouple retry attempts.** The coordinator
  now reads `CONTEXT_ENGINE_FETCH_TIMEOUT` / `_PARSE_TIMEOUT` /
  `_EMBED_TIMEOUT` / `_STORE_TIMEOUT` and wraps each retry attempt
  in `context.WithTimeout`. Previously the *initial* deadline was
  shared across retries, so the second/third attempt inherited a
  stale (near-expired) context — Round-9 makes every attempt
  independent so a flaky sidecar burst genuinely retries instead of
  failing on a 1-second residual deadline.

- **Cache warm-on-miss with `context.WithoutCancel`.** When the
  retrieval handler sees a cache miss and
  `CONTEXT_ENGINE_CACHE_WARM_ON_MISS=true`, the cache write is
  scheduled in a fire-and-forget goroutine using
  `context.WithoutCancel(reqCtx)`. The response returns the moment
  the result is materialised; Redis-write latency never enters
  the user's hot path. The detached context inherits values
  (tenant ID, trace context) but not deadlines or cancellation.

- **gRPC sidecar circuit-breaker Prometheus gauge.** The pool
  (`internal/grpcpool/pool.go`) emits
  `context_engine_grpc_circuit_breaker_state{target}` with the
  spec values `0=closed, 1=half-open, 2=open`. To keep the wire
  protocol stable as new states get added, the emit path uses an
  explicit `gaugeValueForState(State) int` switch — not the
  enum's `iota` value — so renumbering the `State` constants
  cannot silently drift the wire signal.

- **Prometheus recording rules + multi-file `alertcheck`.**
  `deploy/recording-rules.yaml` ships pre-computed series
  (`context_engine_retrieval_availability`, `_p95_latency_ms`,
  `_pipeline_throughput_per_minute`, `_error_rate_per_minute`,
  `_cache_hit_rate`) so Grafana panels render in O(1) lookups
  instead of 5-minute range queries. The `alertcheck` binary now
  accepts multiple manifest paths and recognises the `record`
  rule form (with severity/summary checks correctly scoped to
  alert rules only). `make alerts-check` validates both files.

- **Connection-pool health gauges + sampler goroutine.** Three
  new gauges
  (`context_engine_postgres_pool_open_connections`,
  `_redis_pool_active_connections`,
  `_qdrant_pool_idle_connections`) report live pool stats. A
  `PoolSampler` goroutine started from `cmd/api/main.go` reads
  `db.Stats().OpenConnections`, `redis.PoolStats().TotalConns -
  IdleConns`, and `qdrant.IdleConnCapacity()` every 30s and
  publishes the readings. The Qdrant signal reports the
  configured idle ceiling rather than the live count because
  `net/http.Transport` doesn't expose live idle-conn counts on
  its public surface — a stable baseline matching operator
  capacity is still actionable for alerting.

- **Regression manifest tying PR-#16 + PR-#17 fixes to tests.**
  `tests/regression/round78_manifest.go` extends the Round-4
  regression manifest with each Devin Review fix and the
  regression test that pins it. A meta-test
  (`round78_manifest_test.go`) reads each entry and verifies the
  named source file contains a `func TestX(` declaration —
  so renaming a test without updating the manifest fails CI.

### Tech choices added in Round 8

- **Stage 4 deduplication is now in the hot path.** The
  coordinator constructs a `pipeline.Deduplicator` from the
  configured embedding dimension and the
  `CONTEXT_ENGINE_DEDUP_NEAR_THRESHOLD` cosine cutoff
  (default `0.97`). The store worker calls `Deduplicator.Drop`
  on every chunk before persisting to Qdrant/FalkorDB/Postgres;
  near-duplicate chunks are dropped with an audit-style metric.
  Gated behind `CONTEXT_ENGINE_DEDUP_ENABLED`.

- **Priority buffer in front of Submit.** When
  `CONTEXT_ENGINE_PRIORITY_ENABLED=true`, `Coordinator.Submit`
  routes events through `pipeline.PriorityBuffer`, which
  drains high-priority (steady-state) events before
  low-priority (backfill) events. This guarantees that a
  live-user-triggered ingestion (e.g. a Slack message just
  posted) is never starved by a long-running backfill.

- **Per-source embedding-model overrides.** The Stage 3 embed
  worker consults
  `admin.EmbeddingConfigRepository.Get(sourceID)` and passes
  the resolved model name to the embedding gRPC service.
  When no row exists the worker falls back to the default
  model. This unblocks experiments like “use
  `text-embedding-3-large` for the GitHub source and
  `bge-large` for everything else” without forking the
  pipeline.

- **Retry analytics wired into `runWithRetry`.** Every
  attempt — success, retry, failure — is recorded on
  `pipeline.RetryAnalytics`. `GET
  /v1/admin/pipeline/retry-stats` now returns live data from
  the ingest binary instead of a static placeholder.

- **Notification dispatcher in the audit pipeline.** The
  audit repository is wrapped with a
  `notifyingAuditRepository` that calls
  `NotificationDispatcher.Dispatch` after every successful
  audit-log insert. The dispatcher walks the per-tenant
  notification preferences for the event type, fans the
  payload out to webhook and email targets, and persists
  every attempt in `notification_delivery_log` (with
  `next_retry_at` for retryable failures).

- **All six admin stores are now Postgres-backed.**
  `QueryAnalyticsStoreGORM`, `PinnedResultStoreGORM`,
  `SyncHistoryGORM`, `LatencyBudgetGORM`, `CacheTTLGORM`, and
  `CredentialHealthGORM` use the same GORM patterns as the
  rest of the admin surface. `cmd/api/main.go` no longer
  instantiates the in-memory variants; those remain only as
  test fakes.

- **Retrieval handler hooks for admin-owned state.** Three
  new setters on `retrieval.Handler` decouple the retrieval
  pipeline from `cmd/api`'s wiring:
  - `SetLatencyBudgetLookup` — bounds the request context
    via `context.WithTimeout(req.Context, budget)` so a
    per-tenant `max_latency_ms` actually shortens the
    request deadline.
  - `SetCacheTTLLookup` — consulted on every
    `cache.Set(...)` so per-tenant TTL overrides flow
    through to Redis `EXPIRE`.
  - `SetPinLookup` — invoked after policy filtering and
    before caching; pinned chunks are inserted via
    `pin_apply.ApplyPins` at the configured positions.

- **Notification retry worker.** A new background worker
  (`admin.NotificationRetryWorker`) scans
  `notification_delivery_log` for rows whose `next_retry_at`
  has passed, redelivers them via the configured
  `NotificationDelivery`, and applies linear backoff. Rows
  that exhaust `DefaultMaxRetryAttempts` (5) are
  dead-lettered: `status=failed` with `next_retry_at`
  cleared.

- **Credential health worker.** `cmd/ingest/main.go`
  registers a background goroutine that ticks every
  `CONTEXT_ENGINE_CREDENTIAL_HEALTH_INTERVAL` (default `1h`).
  Each tick lists every active source, invokes
  `connector.Validate()`, persists the outcome to
  `source_health.credential_*`, and emits the audit event
  `source.credential_invalid` on failure (which then fans out
  through the notification dispatcher).

- **CI fast-lane gains alerts-check and rollback-parity.**
  `.github/workflows/ci.yml` adds two PR-blocking jobs:
  `fast-alerts` runs `make alerts-check` (validates
  `deploy/alerts.yaml`) and `fast-rollback-parity` runs the
  `migrations/rollback/...` tests that enforce every forward
  migration has a matching down-script.
