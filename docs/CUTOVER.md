# Cutover plan: Python pipeline → Go context engine

This document plans the Phase 3 → Phase 4 migration from the legacy
Python ingestion / retrieval stack (in `uneycom/ai-agent-context-engine`)
to the Go context engine in this repo (`kennguy3n/hunting-fishball`).
The migration is incremental and feature-flag-gated; rollback is one
config flip.

## 1. Acceptance criteria

The following must hold before the migration starts. All numbers come
from `tests/benchmark/` on the AMD EPYC 7763 (4-core slice):

| Metric                                  | Target              | Measured (Phase 3 baseline) |
| --------------------------------------- | ------------------- | --------------------------- |
| Pipeline throughput (in-process fakes)  | ≥ 50 000 docs/sec   | ~268 000 docs/sec           |
| Pipeline heap delta per `b.N=1000`      | ≤ 50 MB             | ~2.4 MB                     |
| Retrieval P50 (vector + BM25 fan-out)   | ≤ 100 µs in-process | ~50 µs                      |
| Retrieval P95                           | ≤ 500 µs in-process | ~178 µs                     |
| Retrieval P99                           | ≤ 2 ms in-process   | ~407 µs                     |
| `make test` exit code                   | 0                   | 0                           |
| `make test-e2e` exit code               | 0                   | 0                           |
| `make test-integration` exit code       | 0 with services up  | 0                           |

> The in-process numbers are the **handler glue cost**, not full
> end-to-end with real Qdrant / Redis / FalkorDB. Production targets
> from `docs/PROPOSAL.md` (≤120 ms P95 retrieval) are validated in
> `make test-e2e` and the load-test harness in Phase 4.

## 2. Prerequisites

1. The Phase 3 PR is merged on `main` and the `e2e` and `integration`
   CI jobs are green.
2. The three Python ML services
   (`services/docling`, `services/embedding`, `services/memory`) build
   into images on the registry and pass their unit tests in CI.
3. Phase 0/1 storage plane (Postgres, Redis, Kafka, Qdrant) is
   deployed and healthy in the target environment.
4. A FalkorDB instance is reachable and the channel-policy tables are
   seeded with at least the privacy-label ordering.

## 3. Feature flags

The cutover is staged behind a small set of environment variables in
`cmd/api`:

| Variable                          | Default   | Purpose                                      |
| --------------------------------- | --------- | -------------------------------------------- |
| `CONTEXT_ENGINE_BM25_DIR`         | (unset)   | Tantivy index directory; unset = no BM25     |
| `CONTEXT_ENGINE_REDIS_URL`        | (unset)   | Redis URL; unset = no semantic cache         |
| `CONTEXT_ENGINE_FALKOR_ENABLED`   | `0`       | `1` enables FalkorDB graph traversal         |
| `CONTEXT_ENGINE_MEMORY_TARGET`    | (unset)   | Mem0 gRPC target; unset = no memory backend  |

Each backend is independently optional. With every flag unset the
handler degrades to the Phase 1 vector-only behaviour, which is
exactly what the legacy traffic expects.

## 4. Rollout steps

### Step 1 — Shadow mode (read-only)

1. Deploy `cmd/api` with **all Phase 3 flags unset**. The new binary
   runs in vector-only mode.
2. Mirror 1% of production retrieval traffic to the new endpoint via
   the existing API gateway. Compare response shapes (the Go endpoint
   returns the legacy response on cache miss / no-flag config).
3. Run for 24 h. Watch the audit-log error rate and the
   `policy.degraded` field in the Go endpoint's responses.

### Step 2 — Enable BM25

1. Stand up the Tantivy directory (`/var/lib/hf/bm25/`).
2. Run the back-fill job that re-indexes all current chunks into BM25.
3. Set `CONTEXT_ENGINE_BM25_DIR=/var/lib/hf/bm25/`.
4. Watch retrieval P95 from the API metrics. If the new P95 stays
   inside 1.5x the legacy P95, continue. Otherwise unset and roll
   back.

### Step 3 — Enable semantic cache

1. Set `CONTEXT_ENGINE_REDIS_URL=redis://...`.
2. The handler now consults the cache on every retrieve and
   short-circuits on hit. Cache-miss path is identical to Step 2.
3. Watch the cache hit ratio (logged on every retrieve). Target ≥ 30%
   in steady state.

### Step 4 — Enable FalkorDB graph traversal

1. Stand up FalkorDB on the existing Redis instance (FalkorDB is a
   Redis module).
2. Run the back-fill job that materialises graph nodes/edges from the
   document corpus.
3. Set `CONTEXT_ENGINE_FALKOR_ENABLED=1`.
4. Confirm `policy.applied` now lists `graph` for at least 5% of
   queries.

### Step 5 — Enable Mem0

1. Deploy the `services/memory` container.
2. Set `CONTEXT_ENGINE_MEMORY_TARGET=memory:50053`.
3. The pipeline starts writing memory-eligible chunks; the retrieval
   handler now fans out to `memory` as the 4th backend.

### Step 6 — Cut Python pipeline traffic

1. Set the upstream gateway to send 100% of retrieval traffic to the
   Go endpoint.
2. Decommission the Python retrieval API in `ai-agent-context-engine`.
3. Drain the Python ingest pipeline; confirm Kafka offsets advance to
   the head, then decommission.

## 5. Rollback

Each step is independently reversible:

- **Step 5 → 4**: unset `CONTEXT_ENGINE_MEMORY_TARGET`. No data loss
  — Mem0 keeps its store; we just stop reading.
- **Step 4 → 3**: set `CONTEXT_ENGINE_FALKOR_ENABLED=0`. The graph
  data persists for re-enable.
- **Step 3 → 2**: unset `CONTEXT_ENGINE_REDIS_URL`. Cache empties on
  TTL.
- **Step 2 → 1**: unset `CONTEXT_ENGINE_BM25_DIR`. The index stays
  on disk for re-enable.
- **Step 1 → legacy**: flip the gateway back to the Python endpoint.
  The Go binary keeps running in shadow mode.

For a full rollback the dataplane operator runs:

```bash
kubectl set env deploy/context-engine-api \
  CONTEXT_ENGINE_BM25_DIR- \
  CONTEXT_ENGINE_REDIS_URL- \
  CONTEXT_ENGINE_FALKOR_ENABLED=0 \
  CONTEXT_ENGINE_MEMORY_TARGET-
kubectl rollout restart deploy/context-engine-api
```

…and re-points the gateway at the Python service.

## 6. Monitoring checklist

During each step watch the following dashboards / alerts:

- **API**: `/v1/retrieve` P50/P95/P99 latency, error rate, request
  rate per tenant.
- **Policy**: rate of `policy.degraded` non-empty responses (target
  ≤ 1%); rate of `policy.blocked_count > 0` (informational).
- **Cache**: hit ratio per tenant, per channel.
- **Pipeline**: `pipeline.coordinator_*` metrics (submitted, completed,
  skipped, dlq counts).
- **Storage**: Qdrant query latency, Tantivy index size, Redis memory,
  FalkorDB graph size, Postgres connection pool saturation.
- **gRPC**: per-call latency for Docling / embedding / Mem0; circuit
  breaker open count.

If any of the alerts trip, follow the rollback section. Keep the
legacy Python stack warm for two weeks after Step 6 before
decommissioning hardware.

## 7. Post-Phase-3 additions

The sections below cover operational features that post-date this
document's original Phase-3 framing. The cutover plan above is still
correct — these are additive overlays.

### 7.1 Feature flags (all default to *off* / safe)

| Variable                                  | Default | Effect                                                    |
| ----------------------------------------- | ------- | --------------------------------------------------------- |
| `CONTEXT_ENGINE_PRIORITY_BUFFER_ENABLED`  | `0`     | Priority buffer for hot-tenant fairness.                  |
| `CONTEXT_ENGINE_DEDUP_ENABLED`            | `0`     | Content-hash dedup at Stage 4 of the pipeline.            |
| `CONTEXT_ENGINE_CHUNK_ACL_ENABLED`        | `0`     | Per-chunk ACL gating + shard pre-gen filtering.           |
| `CONTEXT_ENGINE_SSE_STREAMING_ENABLED`    | `0`     | Server-Sent Events streaming for /v1/retrieve.            |
| `CONTEXT_ENGINE_CHUNK_SCORING_ENABLED`    | `0`     | Chunk-quality scoring hook (writes `chunk_quality`).      |
| `CONTEXT_ENGINE_QUERY_EXPANSION_ENABLED`  | `0`     | Synonym-driven BM25 expansion.                            |
| `CONTEXT_ENGINE_HOOK_TIMEOUT`             | `500ms` | Timeout guard for Stage-4 GORM hooks.                     |
| (`gracefulLookupTimeout` constant)        | `200ms` | LatencyBudget / CacheTTL / Pin lookup timeout — no env override by design. |

The GORM-backed stores (latency budget, cache TTL, pin list, query
analytics, sync history, chunk quality) are wired unconditionally;
graceful-degradation wrappers ensure a sick store can never block
the retrieve hot path beyond the 200ms deadline.

### 7.2 Rollback procedure

The rollback procedure covers all 43 forward migrations.
`make migrate-rollback` walks `migrations/rollback/NNN_*.down.sql`
in reverse order:

```bash
make migrate-rollback             # rolls back the most recent migration
make migrate-rollback STEPS=5     # rolls back the last 5 migrations
```

Migration ordering is enforced statically by
`migrations/migration_order_test.go`:

- No two forward migrations share a numeric prefix.
- Prefixes are strictly monotonically increasing from `001`.
- Every forward migration has a matching `rollback/NNN_*.down.sql`.

A merge that violates any of these invariants fails CI before it
can reach `main`.

### 7.3 Capacity test

The capacity harness (`tests/capacity/`) is the official load-test
entry point. Run it via:

```bash
make capacity-test
```

The harness fans out concurrent retrievals across N tenants with
configurable mix of cache-warm vs organic traffic, asserts
end-to-end P95 stays under the targets in section 1, and writes a
JSON report to `tests/capacity/last_run.json`.

### 7.4 Cardinality policy

No Prometheus metric may use `tenant_id` as a label. Tenant
identity goes into `slog.With("tenant_id", ...)` log fields
instead. The policy is enforced by
`TestMetrics_NoTenantIDLabel`; violating it fails `make test`.
This keeps the Prometheus cardinality bounded on a multi-tenant
cluster.

### 7.5 Alert rules

Four PrometheusRule entries in `deploy/alerts.yaml`:

- `ChunkQualityScoreDropped` — avg quality < 0.5 over 15m.
- `CacheHitRateLow` — `context_engine_cache_hit_rate` < 0.3 over 15m.
- `CredentialHealthDegraded` — any source has `credential_valid=false` for > 1h.
- `GORMStoreLatencyHigh` — GORM query P95 > 200ms over 5m.

Validate locally with `make alerts-check`.

