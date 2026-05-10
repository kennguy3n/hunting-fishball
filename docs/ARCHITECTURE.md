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
│   │                          # (forget.go)
│   ├── observability/         # Phase 8: OpenTelemetry tracing
│   │                          # helper + attribute-key constants
│   │                          # (tracing.go); used by the pipeline
│   │                          # coordinator and the retrieval
│   │                          # fan-out
│   └── grpcpool/              # Phase 8: round-robin gRPC connection
│                              # pool with per-call deadline +
│                              # circuit breaker (closed → open →
│                              # half-open) for the Python sidecars
├── proto/
│   ├── docling/v1/            # Python Docling parsing service
│   ├── embedding/v1/          # Python embedding service
│   └── memory/v1/             # Mem0 persistent memory service
├── services/                  # Python ML microservices (Phase 3)
│   ├── _proto/                # generated Python gRPC stubs
│   ├── docling/               # Docling gRPC server + Dockerfile
│   ├── embedding/             # sentence-transformers gRPC server
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
│   └── 006_shards.sql         # Phase 5 shards table (manifest +
│                              # version + chunk_count + status)
├── tests/
│   ├── e2e/                   # docker-compose smoke test
│   │                          # (build tag: //go:build e2e)
│   ├── integration/           # Go ↔ Python gRPC integration tests
│   │                          # (build tag: //go:build integration)
│   ├── benchmark/             # pipeline + retrieval benchmarks
│   └── capacity/              # Phase 8 capacity test
│                              # (`make capacity-test`,
│                              # CAPACITY_DOCS_PER_MIN +
│                              # CAPACITY_DURATION env tunables)
├── docs/                      # PROPOSAL / ARCHITECTURE / PHASES /
│                              # PROGRESS / CUTOVER
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
