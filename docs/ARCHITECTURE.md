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
  "policy": { "applied": [...], "blocked_count": 0 },
  "trace_id": "..."
}
```

The same shape is returned on every platform. The privacy label is the
client's source of truth for the privacy strip UI.

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
  Python service CPU / RAM, embedding QPS.
- **Logs.** Structured JSON; every line carries `tenant_id`, `trace_id`,
  `component`. PII is redacted at the log line by the logger middleware.

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
│   │                          # interfaces (DeltaSyncer / WebhookReceiver /
│   │                          # Provisioner), process-global registry
│   ├── credential/            # AES-256-GCM envelope encryption for
│   │                          # connector credentials
│   ├── audit/                 # AuditLog model, repository (transactional
│   │                          # outbox), Kafka outbox poller, Gin handler
│   ├── pipeline/              # (Phase 1) pipeline coordinator
│   ├── retrieval/             # (Phase 3) retrieval API
│   └── storage/               # (Phase 1+) storage clients
├── proto/
│   ├── docling/v1/            # Python Docling parsing service
│   ├── embedding/v1/          # Python embedding service
│   └── memory/v1/             # Mem0 persistent memory service
├── pkg/                       # public shared types (reserved)
├── migrations/
│   └── 001_audit_log.sql      # audit_logs table + indexes
├── docs/                      # PROPOSAL / ARCHITECTURE / PHASES / PROGRESS
├── docker-compose.yml         # local dev: Postgres / Redis / Kafka / Qdrant
├── Makefile                   # build / test / vet / lint / proto-gen
├── .github/workflows/ci.yml   # CI: vet / test / lint / proto-gen check
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
