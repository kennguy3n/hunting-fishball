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

**Status.** ⏳ planned

- [ ] Admin portal flows for connect / pause / re-scope / remove
- [ ] Per-tenant Kafka topic routing keyed on `tenant_id || source_id`
- [ ] Org-wide sync pipeline runs through the Go context engine
- [ ] Per-source quota + rate-limit enforcement at the platform backend
- [ ] Sync health (last-success, lag, error counts) surfaced in admin UI
- [ ] Forget-on-removal worker — fenced lease prevents re-add races
- [ ] Connector lifecycle events emitted to audit log

## Phase 3 — Retrieval fan-out

**Status.** 🟡 partial | ~70%

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
      end-to-end latency target deferred to Phase 4 load tests)

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
| _(empty until Phase 1)_ | | | | | | |

---

## Changelog

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
