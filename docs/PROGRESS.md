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

**Status.** ⏳ planned

- [ ] `SourceConnector` interface defined in the platform backend
- [ ] Optional `DeltaSyncer` / `WebhookReceiver` / `Provisioner` interfaces
- [ ] Process-global connector registry (`Register*` / `Get*`)
- [ ] AES-GCM credential encryption reused from the platform backend
- [ ] Audit-log Postgres table + Kafka outbox
- [ ] Audit-log API surfaced to the admin portal

## Phase 1 — Single-source MVP end-to-end

**Status.** ⏳ planned

- [ ] Google Drive connector implements `SourceConnector` with delta tokens
- [ ] Slack connector implements `SourceConnector` with the Events API
- [ ] Go context engine consumes from Kafka
- [ ] Stage 1 (Fetch) Go worker
- [ ] Stage 2 (Parse) gRPC integration with Python Docling service
- [ ] Stage 3 (Embed) gRPC integration with Python embedding service
- [ ] Stage 4 (Storage) Go worker — Qdrant + Postgres
- [ ] `POST /v1/retrieve` returns top-k matches from Qdrant
- [ ] CI end-to-end smoke test (docker-compose storage plane)

## Phase 2 — B2B Admin Source Management

**Status.** ⏳ planned

- [ ] Admin portal flows for connect / pause / re-scope / remove
- [ ] Per-tenant Kafka topic routing keyed on `tenant_id || source_id`
- [ ] Org-wide sync pipeline runs through the Go context engine
- [ ] Per-source quota + rate-limit enforcement at the platform backend
- [ ] Sync health (last-success, lag, error counts) surfaced in admin UI
- [ ] Forget-on-removal worker — fenced lease prevents re-add races
- [ ] Connector lifecycle events emitted to audit log

## Phase 3 — Retrieval fan-out

**Status.** ⏳ planned

- [ ] Qdrant Go client integrated, per-tenant collections
- [ ] `tantivy-go` integrated, per-tenant directories
- [ ] FalkorDB Go client integrated, per-tenant graphs
- [ ] Mem0 gRPC contract + Go client
- [ ] Retrieval API parallel fan-out via `errgroup` with deadlines
- [ ] Reciprocal-rank fusion merger
- [ ] Cross-encoder reranker + Go fallback reranker
- [ ] Redis semantic cache with lazy invalidation on Stage 4 writes
- [ ] Retrieval P95 < 500 ms on the sample corpus

## Phase 4 — Policy framework + simulator + privacy strip

**Status.** ⏳ planned

- [ ] Tenant- and channel-scoped privacy mode
- [ ] Allow / deny lists (source / namespace / path glob)
- [ ] Recipient policy (channel / skill)
- [ ] What-if simulator
- [ ] Data-flow diff in simulator
- [ ] Conflict detection in simulator
- [ ] Drafts isolated; explicit promotion audited
- [ ] `privacy_label` returned on every retrieval row
- [ ] Privacy strip in admin portal, desktop, and mobile

## Phase 5 — On-device knowledge core integration

**Status.** ⏳ planned

- [ ] UniFFI XCFramework for iOS
- [ ] UniFFI AAR for Android
- [ ] N-API binding for desktop
- [ ] On-device retrieval shard sync
- [ ] Local-first retrieval, policy-bounded fallback
- [ ] Bonsai-1.7B GGUF via `llama.cpp` on at least one desktop + one mobile
- [ ] Cryptographic forgetting on the on-device tier

## Phase 6 — B2C client surfaces

**Status.** ⏳ planned

- [ ] iOS / Android / desktop B2C apps consume the same retrieval API
- [ ] On-device first by default
- [ ] Privacy strip in B2C clients
- [ ] Background sync per platform native scheduler

## Phase 7 — Catalog expansion

**Status.** ⏳ planned

- [ ] ≥ 12 production connectors at GA
- [ ] Per-connector runbooks
- [ ] Per-connector capability matrix in this doc
- [ ] End-to-end smoke test green per connector

## Phase 8 — Cross-platform optimization

**Status.** ⏳ planned

Go context engine tuning:

- [ ] Goroutine pool sizing per stage tuned against measured latency
- [ ] Kafka consumer rebalancing tuned (sticky, session timeout, max poll)
- [ ] Connection pooling for Qdrant / FalkorDB / Tantivy / Postgres
- [ ] HPA on Kafka lag (ingest) and QPS (api)
- [ ] OpenTelemetry trace sampling tuned for cost / tail-latency tradeoff

Python ML microservice scaling:

- [ ] HPA on Docling worker (CPU + queue depth)
- [ ] HPA on embedding worker (CPU + queue depth)
- [ ] Mem0 partitioning by tenant prefix
- [ ] gRPC connection pooling + per-target deadlines on the Go side
- [ ] Capacity test (N docs / min) without back-pressure to connectors

Cross-platform on-device:

- [ ] Bonsai-1.7B benchmarks across ≥ 3 tiers per platform
- [ ] On-device retrieval shard eviction tuned per tier

---

## Context Engine migration tasks

These are the discrete tasks that move the context engine from
"Python pipeline + Python retrieval" to "Go orchestrator + Python ML
microservices behind gRPC". They cross-cut Phases 1–3 and 8.

### Proto / contract definitions

- [ ] Define gRPC proto files for the document parsing service (Docling)
- [ ] Define gRPC proto files for the embedding computation service
- [ ] Define gRPC proto files for the memory service (Mem0)

### Go context engine — pipeline

- [ ] Implement Go Kafka consumer (replacing Python `FastKafkaConsumer`)
- [ ] Implement Go pipeline coordinator with goroutine-based workers
      (replacing Python's `multiprocessing` `ProcessCoordinator`)
- [ ] Implement Go Stage 1 (Fetch) worker — HTTP / S3 + retry / dedupe
- [ ] Implement Go Stage 4 (Storage) worker — Qdrant, FalkorDB,
      PostgreSQL writes; transactional outbox

### Go context engine — retrieval API

- [ ] Implement Go retrieval API with Gin
- [ ] Implement Go vector search client (Qdrant)
- [ ] Implement Go BM25 search (`tantivy-go` bindings)
- [ ] Implement Go graph traversal client (FalkorDB)
- [ ] Implement Go semantic cache (Redis)
- [ ] Implement Go result merger and reranker

### Python ML microservices

- [ ] Build Python Docling gRPC microservice (thin wrapper)
- [ ] Build Python embedding gRPC microservice (thin wrapper)
- [ ] Build Python Mem0 gRPC microservice (thin wrapper)

### Validation

- [ ] Write integration tests for Go ↔ Python gRPC communication
- [ ] Benchmark Go context engine vs Python baseline
      (throughput, P50 / P95 / P99 latency, RSS per worker)
- [ ] Document cutover plan and rollback procedure

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
| _(empty until Phase 1)_ | | | | | | |
