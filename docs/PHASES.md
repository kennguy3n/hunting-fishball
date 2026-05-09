# hunting-fishball — Phase Definitions & Exit Criteria

This document collects the planned milestones in one place so PR reviewers
and operators have a shared vocabulary. The phase model intentionally
mirrors the `ai-agent-platform` connector phase model, so a
connector or pipeline change can move between repos without re-numbering.

A phase is **shippable** only when *all* of its exit criteria are
demonstrably met (test, runbook, metric, or migration as appropriate).
Phases stack — a later phase assumes the invariants of every earlier
phase.

| Status legend |  |
|---------------|--|
| ✅ shipped | The phase is in `main` and exercised in production |
| 🟡 partial | Some exit criteria met; gaps tracked in `PROGRESS.md` |
| ⏳ planned | Not yet started |

> Phase 0 is **🟡 partial** as of 2026-05-09 — the connector contract,
> registry, credential encryption, audit log primitives, and gRPC proto
> contracts have all landed. **Phase 1** is **🟡 partial** as of
> 2026-05-09 — Google Drive + Slack connectors, the Go Kafka consumer,
> the 4-stage Go pipeline, the retrieval API, and the docker-compose
> e2e smoke test have all landed. **Phase 3** is **🟡 partial** as of
> 2026-05-09 — BM25 (bleve), FalkorDB graph, Redis semantic cache,
> RRF merger + reranker, parallel fan-out, the three Python ML
> microservices, integration tests, benchmarks, and the cutover
> plan have all landed; the production P95 < 500 ms acceptance
> criterion is deferred to Phase 4 load tests. Every other phase below
> is currently `⏳ planned`. As phases land, flip the marker and move
> the supporting status row in [`PROGRESS.md`](PROGRESS.md).

---

## Phase 0 — Connector contract, registry, audit primitives  🟡

**Scope.** Define the `SourceConnector` interface and the global registry;
every binary that needs source connectors imports the provider packages
for their `init()` side-effects. Stand up the audit-log primitives in the
platform backend.

**Exit criteria.**

- [x] `SourceConnector` interface with `Validate / Connect / ListNamespaces /
      ListDocuments / FetchDocument / Subscribe / Disconnect`.
- [x] Optional `DeltaSyncer`, `WebhookReceiver`, `Provisioner` interfaces.
- [x] Process-global registry with `RegisterSourceConnector` /
      `GetSourceConnector` (mirror of the platform backend's connector
      factory).
- [x] AES-GCM credential encryption reused from the platform backend.
- [x] Blank-import side-effects in the connector binaries that need
      registration.
- [x] Audit-log Postgres table + outbox into Kafka.
- [x] Audit log surfaced in the admin portal API.

---

## Phase 1 — Single-source MVP end-to-end  🟡

**Scope.** Get one source connector all the way through ingestion, the
4-stage Go context engine, the storage plane, and the retrieval API.
Single tenant, single user, single channel. Exists to prove the contract,
not to be production-ready.

**Connectors.** Google Drive (read-only) and Slack (channels the
service-account is in).

**Exit criteria.**

- [x] Google Drive connector implements `SourceConnector` with delta
      tokens.
- [x] Slack connector implements `SourceConnector` with the Events API.
- [x] Go context engine consumes from Kafka and runs Stage 1 (Fetch),
      Stage 2 (gRPC → Python Docling), Stage 3 (gRPC → Python embedding),
      Stage 4 (Storage to Qdrant + Postgres).
- [x] `POST /v1/retrieve` returns top-k matches from Qdrant for a sample
      query.
- [ ] Round-trip latency P95 < 1 s on the sample corpus. *(measurement
      pending Phase 8 capacity test against a deployed stack.)*
- [x] Audit log records every ingestion + retrieval call.
- [x] One smoke test runs in CI end-to-end (using docker-compose for the
      storage plane).

---

## Phase 2 — B2B Admin Source Management  ⏳

**Scope.** Multi-tenant source management for B2B administrators. Wire the
**org-wide sync pipeline through the Go context engine (Kafka)** so that
an admin connecting a source results in steady-state ingestion across the
whole tenant, not just a single user.

**Exit criteria.**

- [ ] Admin portal flows for connecting / pausing / re-scoping / removing
      a source, all multi-tenant.
- [ ] Per-tenant Kafka topic routing keyed on `tenant_id || source_id`.
- [ ] Org-wide sync pipeline runs through the **Go context engine**
      (replacing any earlier single-user shortcut). The Go consumer
      handles backfill paced separately from steady-state.
- [ ] Per-source quota + rate-limit enforcement at the platform backend
      *before* the connector contacts the external API.
- [ ] Sync health surfaced in the admin portal (last-success, lag,
      error counts) per source per namespace.
- [ ] Forget-on-removal worker drops all derived chunks / embeddings /
      graph nodes / memory entries, with fenced lease so re-add doesn't
      race with deletion.
- [ ] Connector lifecycle events (`source.connected`,
      `source.sync_started`, `chunk.indexed`, `source.purged`) emitted
      to the audit log.

---

## Phase 3 — Retrieval fan-out  🟡

**Scope.** Stand up the four retrieval backends in parallel and merge
their results.

**Exit criteria.**

- [x] Qdrant Go client integrated; per-tenant collection convention
      (landed in Phase 1; reused unchanged here).
- [x] BM25 search integrated via `bleve` (pure-Go fallback for
      `tantivy-go`); per-tenant directory convention.
- [x] FalkorDB Go client integrated; per-tenant graph convention.
- [x] gRPC contract for the Mem0 memory service; Go client integrated
      and wired through the retrieval handler.
- [x] Retrieval API runs all four backends in parallel via `errgroup`,
      with per-backend deadlines and `policy.degraded` signalling.
- [x] Reciprocal-rank fusion merger.
- [x] Go-side lightweight reranker (BM25 blend + freshness boost) +
      `Reranker` interface for a future cross-encoder remote
      reranker.
- [x] Semantic cache in Redis with per-tenant key prefix and explicit
      `Invalidate` on Stage 4 writes.
- [ ] Retrieval P95 latency < 500 ms on the sample corpus
      (in-process P95 ~178 µs measured in `tests/benchmark/`; full
      end-to-end target deferred to Phase 4 load tests).

---

## Phase 4 — Policy framework + simulator + privacy strip  ⏳

**Scope.** Privacy mode, allow / deny lists, recipient policy, and the
policy simulator. Privacy strip surfaces in every client.

**Exit criteria.**

- [ ] Tenant- and channel-scoped privacy mode (`local-only`, `local-api`,
      `hybrid`, `remote`, `no-ai`).
- [ ] Allow / deny lists by source / namespace / path glob.
- [ ] Recipient policy by channel / skill.
- [ ] Policy simulator: what-if retrieval, data-flow diff, conflict
      detection.
- [ ] Drafts isolated from live; explicit promotion is an audited event.
- [ ] `privacy_label` returned on every retrieval row.
- [ ] Privacy strip rendered in admin portal, B2B desktop, and at least
      one mobile platform.

---

## Phase 5 — On-device knowledge core integration  ⏳

**Scope.** Land the Rust knowledge core (`kennguy3n/knowledge`) on mobile
and desktop; wire on-device retrieval shards.

**Exit criteria.**

- [ ] UniFFI bindings for iOS (XCFramework) and Android (AAR).
- [ ] N-API binding for desktop.
- [ ] On-device retrieval shard sync from the Go retrieval API
      (`GET /v1/shards/...`).
- [ ] Local-first retrieval — the client tries the on-device shard
      before contacting the remote API; fallback is policy-bounded.
- [ ] Bonsai-1.7B GGUF integrated via `llama.cpp` on at least one
      desktop and one mobile platform.
- [ ] Cryptographic forgetting on the on-device tier — tenant key
      destruction wipes the local indices and SLM-attached state.

---

## Phase 6 — B2C client surfaces  ⏳

**Scope.** Bring the B2C mobile clients (and desktop companion) to
parity with the on-device-first contract.

**Exit criteria.**

- [ ] iOS / Android / desktop B2C apps consume the same retrieval API
      and the same skill manifest format.
- [ ] On-device first by default; remote retrieval gated behind the
      privacy mode and the device-tier policy.
- [ ] Privacy strip in B2C clients (mobile + desktop).
- [ ] Background sync per platform's native scheduler.

---

## Phase 7 — Catalog expansion  ⏳

**Scope.** Add connectors per the connector catalog in
[`PROPOSAL.md`](PROPOSAL.md#4-connector-catalog). Each connector lands
behind the `SourceConnector` contract and reuses the existing pipeline.

**Exit criteria.**

- [ ] At least 12 production connectors at GA (Phase-1 target met).
- [ ] Each connector has its own runbook for credential rotation,
      quota incidents, and outages.
- [ ] Per-connector capability matrix in [`PROGRESS.md`](PROGRESS.md).
- [ ] Connector code path passes the same end-to-end smoke test that
      Phase 1 introduced.

---

## Phase 8 — Cross-platform optimization  ⏳

**Scope.** Performance tuning for the Go context engine and the Python
ML microservices, plus a cross-platform pass on the on-device tier.

**Exit criteria — Go context engine tuning.**

- [ ] Goroutine pool sizing per stage tuned against measured latency
      (Stage 1 fetch concurrency, Stage 4 storage concurrency).
- [ ] Kafka consumer group rebalancing tuned (sticky assignment, session
      timeout, max poll interval) so re-balances don't stall ingestion.
- [ ] Connection pooling for Qdrant, FalkorDB, Tantivy, and PostgreSQL
      tuned to match the Gin server's expected QPS.
- [ ] Memory ceilings per `context-engine-ingest` and
      `context-engine-api` deployment, with HPA on Kafka lag and QPS
      respectively.
- [ ] OpenTelemetry trace sampling tuned to keep cost under budget
      while still catching tail latency.

**Exit criteria — Python ML microservice scaling.**

- [ ] Horizontal scaling of the Docling and embedding workers
      independently (each with its own HPA on CPU + queue depth).
- [ ] Mem0 service partitioning by tenant prefix to prevent noisy
      neighbours.
- [ ] gRPC connection pooling and per-target deadlines on the Go side.
- [ ] Capacity test that ingests N documents / minute and proves the
      pipeline does not back-pressure the connectors.

**Exit criteria — cross-platform on-device.**

- [ ] Bonsai-1.7B benchmarks across at least three device tiers per
      platform (iOS / Android / desktop), with documented effective-tier
      transitions under thermal pressure.
- [ ] On-device retrieval shard eviction tuned per tier so heavy clients
      do not OOM low-tier devices.

---

## Cross-cutting requirements

These apply to every phase:

- **No regressions.** Each phase ships with a regression suite the next
  phase inherits.
- **Audit log first.** Every new operation lands its audit-log event
  before its happy-path is wired through the UI.
- **Tenant guard.** Every new storage call goes through the tenant-scoped
  storage client; cross-tenant queries are not allowed at the library
  boundary.
- **Privacy contract.** Every new retrieval path returns a
  `privacy_label`. The privacy strip is part of the definition of done.
