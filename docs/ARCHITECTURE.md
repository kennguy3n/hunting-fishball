# hunting-fishball — Architecture

> **Audience.** Engineers building or operating the platform.
>
> **Status.** Active. This document describes the system architecture.
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

**Health checks.** Each Python sidecar registers the
gRPC health protocol (`grpc_health.v1.Health`) at boot. Operators can
probe each service uniformly with `grpc_health_probe -addr=:<port>`,
matching the surface the Go process exposes via `/healthz` / `/readyz`.
This unlocks Kubernetes' built-in `grpc` readiness probe on the Python
pods, replacing the previous TCP-only probe.

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
- **Stage 3b: Entity extraction (gRPC → GraphRAG, optional).** When
  `CONTEXT_ENGINE_GRAPHRAG_ENABLED=true`, the
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

**Bulk retrieval.** Clients populating dashboards
or running multi-query experiments use `POST /v1/retrieve/batch` to
fan out up to 32 sub-requests at once. The handler caps in-flight
work at `max_parallel` (default 8); per-request policy and privacy
isolation is preserved so one failed query does not fail the batch.
Schema: see `docs/openapi.yaml` (BatchRequest / BatchResponse).

**Admin surface.** The admin handlers are grouped
under `/v1/admin/`:

- `POST /v1/admin/sources`, `GET`, `PATCH`, `DELETE`,
  `GET /v1/admin/sources/:id/health` — connector CRUD + health.
- `GET /v1/admin/sources/:id/progress` — per-namespace sync progress
  (`internal/admin/sync_progress_handler.go`).
- `GET /v1/admin/dashboard` — tenant-wide health + throughput +
  P95 + per-backend availability summary
  (`internal/admin/dashboard_handler.go`).
- `GET /v1/admin/dlq`, `GET /v1/admin/dlq/:id`,
  `POST /v1/admin/dlq/:id/replay` — dead-letter inspection + retry
  (`internal/admin/dlq_handler.go`).
- `POST /v1/admin/reindex` — re-emit Stage 2–4 events for an
  existing tenant / source / namespace
  (`internal/admin/reindex_handler.go`).
- `POST /v1/admin/policy/drafts` (+ list / get / promote / reject /
  simulate / simulate/diff / conflicts) — policy framework.
- `DELETE /v1/admin/tenants/:tenant_id` — tenant deletion workflow.
- `GET /v1/admin/audit` (also reachable as `/v1/audit-logs` for
  back-compat) — audit log search/filter with `action=` /
  `resource_id=` / `source_id=` / `q=` / `since=` / `until=`
  (`internal/audit/handler.go`).

**Public-API rate limit.** A Gin middleware on
the `/v1/` group enforces a per-tenant token bucket backed by Redis
when `CONTEXT_ENGINE_API_RATE_LIMIT` is set; HTTP 429 + `Retry-After`
on overage. Falls open on Redis failure so a transient cache outage
does not blackhole the API.

**Request ID middleware.** Every inbound request
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
- **Per-connector runbooks.** Operations runbooks for every
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

### 7.1 Webhook security

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

### 7.2 Connector credential rotation

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

`tests/e2e/degradation_test.go` ships, exercising
the four canonical failure modes in-process (vector down, all
backends down, slow backend exceeding the budget, and Redis cache
down). The retrieval handler is asserted to never return a 5xx and
to always carry `policy.degraded` for any backend that errored.

**Graceful shutdown.** Both binaries trap
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

The code layout below mirrors the running system. Each
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
│   │   │                      # Provisioner), process-global registry.
│   │   │                      # 54 production connectors.
│   │   ├── googledrive/       # Google Drive — change-tokens delta
│   │   ├── slack/             # Slack — Events API + RTM history
│   │   ├── kchat/             # KChat internal chat — channels.changes delta + webhook
│   │   ├── sharepoint/        # SharePoint Online — Graph $delta
│   │   ├── onedrive/          # OneDrive personal — Graph $delta
│   │   ├── dropbox/           # Dropbox v2 — list_folder cursor
│   │   ├── box/               # Box — events stream
│   │   ├── notion/            # Notion — last_edited_time
│   │   ├── confluence/        # Confluence Cloud — CQL delta
│   │   ├── jira/              # Jira Cloud — JQL + webhooks
│   │   ├── github/            # GitHub issues / PRs — webhooks
│   │   ├── gitlab/            # GitLab issues — webhooks
│   │   ├── teams/             # Microsoft Teams — Graph $delta + change notifications
│   │   ├── s3/                # S3-compatible object storage — stdlib SigV4
│   │   ├── linear/            # Linear GraphQL — updatedAt filter + webhooks
│   │   ├── asana/             # Asana tasks — modified_since
│   │   ├── discord/           # Discord channel messages — snowflake cursor
│   │   ├── salesforce/        # Salesforce SOQL — SystemModstamp filter
│   │   ├── hubspot/           # HubSpot CRM v3 — hs_lastmodifieddate search
│   │   ├── mattermost/        # Mattermost chat — since=<ms-epoch>
│   │   ├── clickup/           # ClickUp v2 REST — date_updated_gt
│   │   ├── monday/            # Monday.com GraphQL — __last_updated__
│   │   ├── pipedrive/         # Pipedrive REST v1 — /recents?since_timestamp
│   │   ├── okta/              # Okta Management API — lastUpdated filter
│   │   ├── gmail/             # Gmail REST — history.list historyId cursor
│   │   ├── rss/               # Generic RSS / Atom — max pubDate cursor
│   │   ├── confluence_server/ # Confluence Server / Data Center — CQL lastModified
│   │   ├── entra_id/          # Microsoft Entra ID — Graph $deltatoken
│   │   ├── google_workspace/  # Google Workspace Directory — updatedMin filter
│   │   ├── outlook/           # Microsoft 365 Outlook — Graph @odata.deltaLink
│   │   ├── workday/           # Workday REST — Updated_From filter
│   │   ├── bamboohr/          # BambooHR — /employees/changed?since
│   │   ├── personio/          # Personio — OAuth client-credentials + updated_from
│   │   ├── sitemap/           # sitemap.xml crawl — multi-format <lastmod>
│   │   ├── coda/              # Coda — pageToken walk
│   │   ├── sharepoint_onprem/ # SharePoint Server / on-prem — REST + Modified cursor
│   │   ├── azure_blob/        # Azure Blob Storage — SAS / shared-key, blob-last-modified
│   │   ├── gcs/               # Google Cloud Storage — JSON API + timeCreated/updated
│   │   ├── egnyte/            # Egnyte — events cursor delta
│   │   ├── bookstack/         # BookStack wiki — /api/pages updated_at sort
│   │   ├── upload_portal/     # Signed-upload portal — HMAC-verified multipart WebhookReceiver
│   │   ├── zendesk/           # Zendesk Support — incremental ticket export
│   │   ├── servicenow/        # ServiceNow Table API — sys_updated_on cursor
│   │   ├── freshdesk/         # Freshdesk REST v2 — updated_since
│   │   ├── airtable/          # Airtable REST — LAST_MODIFIED_TIME() filter
│   │   ├── trello/            # Trello REST — actions since cursor
│   │   ├── intercom/          # Intercom REST v2 — conversations search
│   │   ├── webex/             # Webex Messages — before/max cursor
│   │   ├── bitbucket/         # Bitbucket Cloud — updated_on cursor
│   │   ├── quip/              # Quip — updated_usec microsecond cursor
│   │   ├── freshservice/      # Freshservice — updated_since + page-based
│   │   ├── pagerduty/         # PagerDuty — since=<ISO8601> + more/offset
│   │   └── zoho_desk/         # Zoho Desk — modifiedTimeRange + orgId header
│   ├── credential/            # AES-256-GCM envelope encryption for connector creds
│   ├── audit/                 # AuditLog model, transactional outbox, Kafka poller, Gin handler
│   ├── admin/                 # source-management API, simulator, DLQ, reindex, sync-progress,
│   │                          # per-tenant API ratelimit, tenant-deletion workflow,
│   │                          # connector-health + analytics handlers
│   ├── policy/                # privacy modes / ACLs / recipient policy, simulator (snapshot,
│   │                          # diff, conflict, draft FSM, audited promotion), GORM live
│   │                          # store + resolver, device-first hint
│   ├── pipeline/              # 4-stage pipeline (Fetch → Parse → Embed → Store) with
│   │                          # producer / consumer / coordinator, dedup, priority buffer,
│   │                          # per-stage breakers, retention worker, reindex orchestrator,
│   │                          # DLQ observer
│   ├── retrieval/             # POST /v1/retrieve fan-out (vector + BM25 + graph + memory),
│   │                          # RRF merger, reranker, semantic cache, policy snapshot
│   │                          # resolver, privacy-strip enrichment, batch / explain / stream
│   ├── storage/               # Qdrant, Postgres, Tantivy (BM25 via bleve), FalkorDB,
│   │                          # Redis semantic cache, content-hash dedup
│   ├── shard/                 # shard manifest API + delta protocol + cryptographic-forgetting
│   │                          # orchestrator + GORM coverage repo + per-tier eviction
│   ├── models/                # model catalog (ModelEntry / Provider) + GET /v1/models/catalog
│   ├── b2c/                   # B2C client SDK bootstrap (/v1/health, /v1/capabilities,
│   │                          # /v1/sync/schedule)
│   ├── observability/         # OpenTelemetry tracing, Prometheus collectors, Gin middleware,
│   │                          # structured slog logger with W3C traceparent + tenant context
│   ├── grpcpool/              # round-robin gRPC pool with per-call deadlines + circuit breaker
│   ├── lifecycle/             # ordered Step.Add() shutdown runner with deadline budget
│   ├── config/                # startup env validation (ConfigError aggregator)
│   ├── migrate/               # SQL migration runner (schema_migrations + DryRun)
│   ├── eval/                  # retrieval evaluation harness (Precision@K / Recall@K / MRR / NDCG)
│   ├── errors/                # structured error catalog + Gin middleware → wire JSON
│   └── util/                  # internal helpers (e.g. util/strutil cursor / pagination)
├── proto/
│   ├── docling/v1/            # Python Docling parsing service
│   ├── embedding/v1/          # Python embedding service
│   ├── graphrag/v1/           # GraphRAG entity extraction (Stage 3b)
│   └── memory/v1/             # Mem0 persistent memory service
├── services/                  # Python ML microservices (gRPC)
│   ├── _proto/                # generated Python gRPC stubs
│   ├── docling/               # Docling gRPC server + Dockerfile
│   ├── embedding/             # sentence-transformers gRPC server
│   ├── graphrag/              # GraphRAG entity + relation extraction
│   ├── memory/                # Mem0 gRPC server + Dockerfile
│   └── gen_protos.sh          # regenerates _proto/ from proto/
├── pkg/                       # public shared types (reserved)
├── migrations/                # 43 versioned SQL migrations (audit_log, sources, source_health,
│   │                          # policy, policy_drafts, shards, channel_deny_local,
│   │                          # tenant_status, dlq_messages, retention_policy, eval_suites,
│   │                          # sync_schedules, tenant_usage, query analytics, pinned results,
│   │                          # connector templates, synonyms, notifications, latency /
│   │                          # chunk-quality / cache-config, sync history, audit retention,
│   │                          # slow queries, payload limits, DLQ category, chunk_embedding_version,
│   │                          # document_content_type, connector_sync_cursors).
│   └── rollback/              # per-migration *.down.sql; parity enforced by migration_order_test.go
├── tests/
│   ├── e2e/                   # docker-compose smoke tests (build tag `e2e`)
│   ├── integration/           # Go ↔ Python gRPC integration tests (build tag `integration`)
│   ├── benchmark/             # pipeline + retrieval benchmarks
│   ├── regression/            # regression manifest + meta-tests
│   └── capacity/              # capacity harness (`make capacity-test`)
├── docs/                      # PROPOSAL / ARCHITECTURE / PHASES / PROGRESS / CUTOVER
│   ├── runbooks/              # per-connector ops runbooks (credential rotation, quota,
│   │                          # outage detection, error codes)
│   └── contracts/             # client-side contracts: uniffi-ios.md, uniffi-android.md,
│                              # napi-desktop.md, local-first-retrieval.md,
│                              # bonsai-integration.md, b2c-retrieval-sdk.md,
│                              # privacy-strip-render.md, background-sync.md
├── deploy/                    # HorizontalPodAutoscaler manifests, Prometheus alert rules
├── scripts/                   # contributor / CI helpers (`doctor.sh`, `migrate-dry-run-pg.sh`)
├── docker-compose.yml         # local dev: Postgres / Redis / Kafka / Qdrant / FalkorDB /
│                              # Docling / embedding / memory
├── Makefile                   # build / test / vet / lint / proto-gen / e2e / integration /
│                              # bench / eval / alerts-check / migrate-dry-run / doctor /
│                              # fuzz / capacity-test
├── .github/workflows/ci.yml   # CI fast lane gated by `fast-required` aggregator + full
│                              # lane (e2e / integration / connector smoke / capacity /
│                              # migrate-dry-run-pg) + nightly fuzz
├── go.mod
└── go.sum
```

### Tech choices — platform foundations

- **Web framework:** `github.com/gin-gonic/gin`.
- **ORM:** `gorm.io/gorm` with `gorm.io/driver/postgres` in production
  and `github.com/glebarez/sqlite` in repository tests.
- **Kafka client:** `github.com/IBM/sarama`.
- **gRPC:** `google.golang.org/grpc` + `google.golang.org/protobuf`; in-cluster traffic
  uses `NewClient` with `insecure.NewCredentials()`.
- **ULIDs:** `github.com/oklog/ulid/v2`.
- **Crypto:** `crypto/aes` + `crypto/cipher` from the standard library; the envelope
  format mirrors `ai-platform-backend-go/pkg/crypto/encryption` so payloads stay
  cross-compatible across services.
- **Vector store client:** stdlib `net/http` + JSON against the Qdrant REST API;
  per-tenant collections with a hard tenant filter on every search.
- **Postgres driver in production:** `gorm.io/driver/postgres`.

### Tech choices — connector framework

- **HTTP clients:** stdlib `net/http` only with `http.NewRequestWithContext` —
  no vendor SDKs. The same `httptest.Server` test machinery covers every
  connector. Common error sentinels live in `internal/connector/`:
  `ErrInvalidConfig`, `ErrNotSupported`, and `ErrRateLimited` (HTTP 429 wrap).
- **Connector contract:** every connector implements `SourceConnector` plus the
  optional `DeltaSyncer`, `WebhookReceiver`, and `Provisioner` interfaces.
  Connectors are registered in the process-global registry and blank-imported
  by `cmd/api/main.go` and `cmd/ingest/main.go`.
- **Auth models in practice:** OAuth bearer (Microsoft Graph, Google, Notion,
  Linear, Asana, Salesforce, HubSpot, Box, Intercom, Webex), API token (Slack,
  KChat, Confluence Cloud, Jira, GitHub, GitLab, Zendesk, ServiceNow, PagerDuty,
  Quip), API key as basic-auth user with password `"x"` (BambooHR, Freshdesk,
  Freshservice), shared secret + `orgId` header (Zoho Desk), HMAC-verified
  webhook (signed-upload portal), SAS / shared-key signed REST (Azure Blob).
- **Delta sync patterns:** Microsoft Graph `$deltatoken` / `@odata.deltaLink`
  rotation, RFC3339 / ISO-8601 `updated_since` filters, microsecond `updated_usec`
  cursors (Quip), epoch-millisecond `since=<ms>` cursors (Mattermost), snowflake
  `after=<id>` cursors (Discord), Linear / Monday GraphQL `updatedAt` filters,
  Salesforce `SystemModstamp`, HubSpot `hs_lastmodifieddate`, Gmail `historyId`,
  Bitbucket / Trello / Webex page cursors. Empty cursors bootstrap to "now"
  without replaying history; that invariant is pinned by
  `internal/connector/bootstrap_audit_test.go`.
- **Rate limiting:** every connector wraps HTTP 429 (and HTTP-200 GraphQL
  complexity-exception envelopes for Monday) as `connector.ErrRateLimited`
  with `Retry-After` honoured. The retry / backoff lives in the pipeline, not
  the connector.
- **Completeness audit:** `internal/connector/audit_test.go` is a process-global
  CI gate. It fails the build if any connector source drops `ErrInvalidConfig`,
  `ErrNotSupported`, `ErrRateLimited`, or `http.NewRequestWithContext`.
- **Smoke / runbook gates:** `tests/e2e/connector_smoke_test.go` pins the
  registry to at least 54 entries and drives a full
  Validate → Connect → ListNamespaces → ListDocuments → FetchDocument lifecycle
  per connector. `docs/runbooks/runbook_test.go` enforces a matching per-connector
  runbook with the four required sections (credential rotation, quota /
  rate-limit incidents, outage detection, and error-code surface).
- **Schema validation:** `internal/connector/schema_validator.go` exposes
  `CredentialSchemaProvider` + `ValidateCredentialsErr` (object / required /
  properties / type / enum / additionalProperties) wired into
  `POST /v1/admin/sources/preview` before `Validate()` is called.

### Tech choices — pipeline

- **Stages:** the 4-stage Fetch → Parse → Embed → Store pipeline in
  `internal/pipeline/` is wired as a coordinator over goroutines + channels.
  Stage 2 (Parse) is a gRPC client to the Python Docling sidecar; Stage 3
  (Embed) is a gRPC client to the Python embedding sidecar with a
  `RemoteEmbedder` seam so external embedding APIs can plug in behind
  per-tenant policy.
- **Per-tenant Kafka routing:** Sarama `SyncProducer` keys partitions with
  `tenant_id||source_id` (`pipeline.PartitionKey`). The consumer rejects
  body/key mismatches as poison messages and surfaces single-pipe legacy keys
  via `ConsumerConfig.OnLegacyKey` so production can metric the rate.
- **Backfill rate control:** `pipeline.RateController` interface decouples the
  orchestrator from the limiter implementation. `pipeline.TickerRate` paces
  tests; production wiring uses `admin.BoundController` over the Redis token
  bucket.
- **Per-source rate limiter:** atomic Lua token bucket in Redis keyed by
  `(tenant_id, source_id)`. Loaded once via `ScriptLoad` with an `Eval`
  fallback for `NOSCRIPT` after a Redis flush.
- **Dedup, priority, and ACL hooks (flag-gated):**
  `CONTEXT_ENGINE_DEDUP_ENABLED` enables content-hash dedup at Stage 4;
  `CONTEXT_ENGINE_PRIORITY_BUFFER_ENABLED` enables a priority buffer for
  hot-tenant fairness; `CONTEXT_ENGINE_CHUNK_ACL_ENABLED` gates per-chunk ACLs
  + shard pre-gen filtering.
- **Stage-4 hook framework:** `CONTEXT_ENGINE_CHUNK_SCORING_ENABLED` enables
  the chunk-quality scoring hook; `CONTEXT_ENGINE_HOOK_TIMEOUT` (default
  `500ms`) bounds GORM hook latency so a sick store can never stall the hot
  path.
- **Stage breakers and graceful backpressure:** per-stage circuit breakers
  open / half-open / close on stage error rate and feed a Prometheus dashboard.
  `coordinator.PausedSourceFilter` lets Stage 1 drain in-flight work cleanly
  when a source is auto-paused, instead of retrying against the paused
  upstream.
- **Auto-pauser:** `internal/admin/source_auto_pause.go` watches a sliding
  window error rate and additionally honours
  `CONTEXT_ENGINE_SOURCE_AUTO_PAUSE_THRESHOLD` for a consecutive-failure
  fast-stop. The audit row records `trigger=error_rate` vs
  `trigger=consecutive_failures`.
- **Retention worker:** `internal/pipeline/retention_worker.go` periodically
  sweeps Qdrant / FalkorDB / Tantivy / Postgres against the layered
  tenant / source / namespace retention rules in
  `migrations/010_retention_policy.sql`.
- **Reindex orchestrator:** `internal/pipeline/reindex.go` enumerates
  documents by `(tenant_id, source_id, [namespace_id])` and re-emits
  Stage 2-4 events without re-fetching.
- **DLQ surface:** `migrations/009_dlq_messages.sql` persists every
  dead-letter envelope. The DLQ admin handler exposes
  `GET /v1/admin/dlq*`, replay enforces `max_retries`, the DLQ analytics
  endpoint nests a `by_connector_category` rollup, and
  `migrations/040_dlq_category.sql` adds a `category` column
  (`transient` / `permanent` / `unknown`) so the auto-replayer skips
  permanent rows.
- **Webhook HMAC verifier:** `internal/connector/webhook_verify.go` is the
  shared HMAC-SHA256 / SHA-1 / token verifier wired into every connector that
  implements `WebhookReceiver`.

### Tech choices — retrieval

- **BM25 search:** `github.com/blevesearch/bleve/v2` — pure-Go full-text
  index, kept behind a small interface so the backend can swap to
  `tantivy-go` later without changing the retrieval handler. The "Go binary,
  no native deps" invariant rules out the Rust toolchain at build time.
- **Graph traversal:** `github.com/FalkorDB/falkordb-go` (FalkorDB is a Redis
  module that speaks `GRAPH.*` commands). One graph per tenant, named after
  the tenant id.
- **Redis client / semantic cache:** `github.com/redis/go-redis/v9`. Cache
  key is a SHA-256 over `(tenant_id, channel_id, query_embedding,
  scope_hash)` with a per-tenant key prefix; tests use
  `github.com/alicebob/miniredis/v2`. The cache is explicitly invalidated by
  Stage 4 writes via `SemanticCache.Invalidate(chunkIDs)` and per-source via
  `SemanticCache.InvalidateBySources(sourceIDs)`. `GetOrRefresh` does
  cache-aside background refresh.
- **Errgroup fan-out:** `golang.org/x/sync/errgroup` with a per-call context
  derived from per-backend deadlines. Backend errors are logged and surfaced
  as `policy.degraded`, never as a 5xx. Per-backend timing rides on
  `RetrieveResponse.Timings` (vector_ms / bm25_ms / graph_ms / memory_ms /
  merge_ms / rerank_ms).
- **Merger and reranker:** reciprocal-rank fusion merger with a Go-side
  lightweight reranker (BM25 blend + freshness boost) and a `Reranker`
  interface for a future cross-encoder. The gRPC cross-encoder reranker
  sidecar (`proto/reranker/v1/`, `services/reranker/`) is wired behind the
  same interface for tail demotion.
- **Diversity, dedup, and query expansion (flag-gated):** batch-level
  diversity in `merger.go`, cross-backend dedup in `dedup.go`, and
  synonym-driven BM25 expansion gated behind
  `CONTEXT_ENGINE_QUERY_EXPANSION_ENABLED`.
- **Cache warming and pinning:** the `/v1/admin/retrieval/warm-cache` and
  pinned-results admin endpoints (GORM-backed) keep hot queries pre-warmed
  and let admins force results to the top of the rerank slate.
- **Chunk merging:** `internal/retrieval/chunk_merger.go` does post-rerank
  adjacent-short-chunk merging gated behind
  `CONTEXT_ENGINE_CHUNK_MERGE_ENABLED`.
- **Explain and stream:** `/v1/retrieve/explain` returns per-chunk scoring
  breakdowns; `/v1/retrieve/stream` is the SSE streaming surface gated
  behind `CONTEXT_ENGINE_SSE_STREAMING_ENABLED`.
- **Bulk retrieval:** `/v1/retrieve/batch` fans concurrent retrievals with a
  configurable `max_parallel` cap (default 8, hard limit 32) and isolates
  per-request policy so one failed query does not fail the batch.

### Tech choices — policy framework

- **Privacy modes:** `internal/policy/privacy_mode.go` defines a total order
  over `no-ai` < `local-only` < `local-api` < `hybrid` < `remote`.
  `EffectiveMode` returns the stricter of the channel-level and
  tenant-level modes.
- **ACLs and recipient policy:** allow / deny ACLs (`acl.go`) and recipient
  policy (`recipient.go`) compose with privacy modes; they are wired into
  the retrieval handler via `policy_snapshot.go`.
- **Simulator:** `simulator.go` is the what-if engine over a
  copy-on-write `PolicySnapshot.Clone()`; `simulator_diff.go` returns
  per-tier counts and a percentage data-flow delta; `simulator_conflict.go`
  detects privacy-mode overrides, ACL overlaps, and recipient-policy
  contradictions.
- **Drafts and audited promotion:** `draft.go` is the GORM-backed draft
  store (`migrations/005_policy_drafts.sql`), isolated from the live
  resolver. `promotion.go` is the audited promotion FSM with
  `policy.promoted` / `policy.rejected` audit actions; the audit row rides
  the outer transaction via `AuditWriter.CreateInTx`, so a `LiveStore`
  failure rolls the audit row back along with the rest.
- **Live store + resolver:** `live_store.go` applies snapshots
  transactionally to `tenant_policies`, `channel_policies`,
  `policy_acl_rules`, and `recipient_policies` in `004_policy.sql`.
  `live_resolver.go` is the read counterpart used by both retrieval and
  the simulator.
- **Device-first hint:** `internal/policy/device_first.go::Decide` consumes
  `channel_policies.deny_local_retrieval` (added in
  `migrations/007_channel_deny_local.sql`) so the `channel_disallowed`
  reason round-trips end-to-end.
- **Privacy-strip enrichment:** `internal/retrieval/privacy_strip.go`
  enriches every retrieval response with the privacy label that fired.

### Tech choices — admin & operations

- **Source-management API:** Gin handlers in `internal/admin/` scoped under
  the auth middleware group, backed by GORM. `migrations/002_sources.sql`
  + `003_source_health.sql` model sources and source health.
- **Source-health tracking:** a separate `source_health` Postgres table
  keeps `last_success_at`, `last_failure_at`, `lag`, `error_count`, and a
  derived `status` column. `admin.DeriveStatus` computes the status from
  configurable thresholds.
- **Forget-on-removal worker:** a Redis `SET NX EX` fenced lease prevents
  concurrent forget-job runs from racing a re-add. The worker fans out to a
  list of `ForgetSweeper` implementations (one per storage tier).
- **Bulk source surface:** `POST /v1/admin/sources/bulk` runs concurrent
  source operations under one audit envelope.
  `POST /v1/admin/sources/bulk-reindex` and
  `POST /v1/admin/sources/bulk-rotate` are self-documenting aliases that
  funnel through the same concurrency / audit pipeline.
- **Credential rotation:** `POST /v1/admin/sources/:id/rotate-credentials`
  validates new credentials via the connector's `Validate` before swapping;
  the previous credential is held for `CredentialGracePeriod` (1h) so
  in-flight requests drain.
- **GORM-backed stores:** synonyms, retrieval experiments, notification
  subscriptions, connector templates, latency budgets, cache TTLs, pinned
  results, query analytics, sync history, chunk quality, audit retention,
  api_keys, and slow-query analytics each have a dedicated GORM store
  reachable via `/v1/admin/*` endpoints and protected by a graceful-degrade
  lookup wrapper with a 200 ms timeout.
- **DLQ replay and audit retention:** `internal/admin/dlq_handler.go`
  surfaces DLQ entries with replay (`POST /v1/admin/dlq/:id/replay`);
  cursor-aware pagination flows through `util/strutil`. The audit-retention
  worker walks the audit table on a schedule.
- **Tenant deletion:** `internal/admin/tenant_delete.go` runs a 5-step
  `TenantDeleter` workflow + `DELETE /v1/admin/tenants/:tenant_id` backed by
  `migrations/008_tenant_status.sql`.
- **Sync progress:** `GET /v1/admin/sources/:id/progress` returns
  discovered / processed / failed / percent_done per namespace, backed by
  `internal/admin/sync_progress.go`.
- **Billing webhook + per-tenant usage:** `internal/admin/billing_webhook.go`
  exposes `POST /v1/admin/webhooks/billing` (plus GET + DELETE) with
  HTTPS-only URL validation, 32-char minimum shared-secret enforcement,
  audit-logged lifecycle, and a fakeable `BillingWebhookStore` seam.
  `migrations/014_tenant_usage.sql` rolls daily tenant usage.
- **Connector-health and analytics dashboards:**
  `GET /v1/admin/connectors/health` aggregates per-connector-type
  healthy/degraded/failing/paused counts plus average lag and error rate.
  `GET /v1/admin/analytics/connector-health` adds a `window` query
  parameter (`1h` / `24h` / `7d` / `30d`) with a locked response shape
  (`tenant_id`, `window`, `as_of`, `summary`) so a future time-series
  backend can be swapped behind it without breaking clients.
- **Tenant onboarding wizard:** `POST /v1/admin/tenants/:tenant_id/onboarding`
  drives the source-by-source onboarding flow.
- **MessageProbes on /v1/admin/health/summary:** stale connectors, DLQ
  growth, embedding-model availability, and Tantivy disk usage each have a
  probe.

### Tech choices — observability & CI

- **Tracing:** `go.opentelemetry.io/otel` with attribute-key constants in
  `internal/observability/tracing.go`. The pipeline coordinator and the
  retrieval fan-out are instrumented; trace ids round-trip through the
  W3C `traceparent` header.
- **Metrics:** Prometheus collectors in `internal/observability/metrics.go`
  exposed via a Gin middleware at `/metrics` on `cmd/api` and `cmd/ingest`.
  Cardinality policy: **no metric may use `tenant_id` as a label** — tenant
  identity goes into `slog.With("tenant_id", ...)` log fields instead.
  `TestMetrics_NoTenantIDLabel` fails the build on violation.
- **Recording rules:** retrieval P50/P95/P99 and per-stage pipeline timings
  are aggregated by recording rules so dashboards do not pay the histogram
  cost.
- **Pool gauges:** Postgres / Redis / gRPC pool utilisation is exposed as
  Prometheus gauges.
- **Alerts:** `deploy/alerts.yaml` (validated by `make alerts-check`)
  ships PrometheusRule entries for IngestionLagHigh, DLQRateHigh,
  RetrievalP95High, SourceUnhealthy, ChunkQualityScoreDropped,
  CacheHitRateLow, CredentialHealthDegraded, GORMStoreLatencyHigh, and
  per-stage pipeline duration.
- **Health summary:** `GET /v1/admin/health/summary` aggregates pipeline,
  retrieval, connector, and storage probes into a single signal for
  external monitors.
- **Logging:** `internal/observability/logger.go` wraps `log/slog` with a
  `tenant_id` + `trace_id` JSON handler. `GinLoggerMiddleware` injects the
  request-scoped logger from the W3C `traceparent` + tenant context.
- **gRPC pool:** `internal/grpcpool/` provides a round-robin pool with
  per-call deadlines and a closed → open → half-open circuit breaker for
  the Python sidecars. `health.v1` is wired so the pool reads readiness.
- **Lifecycle:** `internal/lifecycle/shutdown.go` provides an ordered
  `Step.Add()` runner with a deadline budget. `cmd/api` drains in-flight
  HTTP then closes Postgres / Redis; `cmd/ingest` stops the consumer,
  drains the pipeline coordinator, then closes the HTTP probe and
  Postgres / Redis. Configurable via
  `CONTEXT_ENGINE_SHUTDOWN_TIMEOUT_SECONDS`.
- **Startup config validation:** `internal/config/validate.go` aggregates
  required-env-var + URL-format errors into a single structured
  `ConfigError`. `ValidateAPI` / `ValidateIngest` run before any
  `gorm.Open` / `redis.NewClient` call.
- **Migration runner:** `internal/migrate/runner.go` reads
  `migrations/NNN_name.sql` in order, tracks applied migrations in
  `schema_migrations`, and supports `DryRun`. Wired behind `AUTO_MIGRATE`.
  `migrations/migration_order_test.go` enforces strict monotonic prefixes
  and forward/rollback parity; `scripts/migrate-dry-run-pg.sh` replays
  every up + rollback migration against a disposable Postgres 16 container
  to catch PG-specific syntax the SQLite dry-run misses.
- **CI fast lane** (`.github/workflows/ci.yml`): `fast-check`, `fast-test`,
  `fast-build`, `fast-lint`, `fast-eval`, `fast-alerts`,
  `fast-rollback-parity`, `fast-migrate-dry-run`, `fast-proto-check`,
  `fast-python`, `fast-connector-unit`, `fast-connector-integration`,
  `fast-regression`, `fast-govulncheck`, and `fast-openapi`, all gated by
  the `fast-required` aggregator (which treats `skipped` as `success` so
  path-filtered lanes do not block doc-only PRs).
- **CI full lane:** `full-proto-gen`, `full-e2e`, `full-integration`,
  `full-connector-smoke`, `full-bench-e2e`, `full-capacity-test`,
  `full-migrate-dry-run-pg`, plus a nightly `nightly-fuzz`.
- **Path-filtered CI:** a `detect-changes` job using
  `dorny/paths-filter@v3` gates `fast-connector-unit`,
  `fast-connector-integration`, and `fast-regression` behind their actual
  paths on pull requests.
- **Unified Go build cache:** every fast-lane Go job uses the same primary
  cache key `${{ runner.os }}-go-build-${{ hashFiles('**/go.sum') }}` with
  job-specific suffixes retained as `restore-keys` fallback, so any green
  fast-lane run warms the build cache for every other job.
- **make doctor:** `scripts/doctor.sh` walks the developer prereq
  checklist (Go ≥ 1.25, Docker, docker-compose, Python 3.11+, protoc,
  golangci-lint, e2e env vars) for `make doctor`.

### Tech choices — on-device tier

- **On-device runtime:** the Rust Knowledge Core exposes a UniFFI surface
  for iOS (Swift / SwiftUI) and Android (Kotlin / Compose), and an N-API
  surface for the Electron desktop client. The same contract lives in
  `internal/shard/contract.go` (Go-side) and the matching Markdown
  contracts under `docs/contracts/` (uniffi-ios.md, uniffi-android.md,
  napi-desktop.md, local-first-retrieval.md, bonsai-integration.md,
  b2c-retrieval-sdk.md, privacy-strip-render.md, background-sync.md).
- **Shard manifest API:** `internal/shard/` exposes the manifest endpoint
  (`handler.go` + `repository.go`), a generation worker (`generator.go`),
  the delta protocol (`delta.go`), and a `coverage` endpoint backed by
  GORM (`coverage_repo.go`). The version-lookup adapter feeds the
  device-first hint in `internal/policy/device_first.go`.
- **Cryptographic forgetting:** `internal/shard/forget.go` orchestrates the
  per-tier forgetting workflow, with a per-tier `eviction.go` policy.
- **SLM:** Bonsai-1.7B via `llama.cpp` (CPU / Metal / CUDA / Vulkan / NPU
  backends). The on-device tier picks the highest-quality backend the
  device's reference profile supports — see `docs/PROPOSAL.md` §9 for the
  reference-device thresholds.
- **B2C client SDK:** `internal/b2c/handler.go` mounts `/v1/health`,
  `/v1/capabilities`, and `/v1/sync/schedule`. `Capabilities.EnabledBackends`
  is pinned to render `[]` even when no backends are configured.
- **Model catalog:** `internal/models/catalog.go` exposes `ModelEntry`,
  `ModelCatalog`, `Provider`, and `StaticProvider`; `handler.go` mounts
  `GET /v1/models/catalog`.
- **Multimodal prep:** `DocumentContentType` (`omitempty` JSON, plus
  `migrations/042_document_content_type.sql`) and
  `IngestEvent.ContentType` tag non-text artifacts so a future multimodal
  router can dispatch to specialised parse stages without re-sniffing the
  MIME — without breaking byte-level wire compatibility for text events.
