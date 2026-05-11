# Operational Runbook — Rounds 6, 7, 8

This runbook covers operator-facing procedures for the
admin surfaces shipped in Rounds 6–8. Each section lists the
relevant endpoint(s), the environment variables, and the
recovery / verification steps an on-call should run.

## 1. Warming the retrieval cache

**Endpoint:** `POST /v1/admin/retrieval/warm-cache`

The cache warm handler accepts an explicit list of queries
*or* `auto_top_n` (default 10) which pulls the most frequent
queries from `query_analytics` and warms the semantic cache
for them.

```bash
curl -X POST $API/v1/admin/retrieval/warm-cache \
  -H "X-Tenant-ID: $TENANT" \
  -H "Content-Type: application/json" \
  -d '{"queries":["status of order 42","reset my password"]}'
```

Verify by issuing the same `/v1/retrieve` requests and
confirming `cache_hit=true` rows appear in
`/v1/admin/analytics/queries` for that tenant.

## 2. Using the A/B testing framework

**Endpoints:**
- `POST /v1/admin/retrieval/experiments` — create an experiment
  with control + variant configs.
- `GET  /v1/admin/retrieval/experiments` — list active.
- `GET  /v1/admin/retrieval/experiments/{name}/results` —
  per-arm aggregates.

The handler invokes the A/B router on every `/v1/retrieve`
request via `Handler.SetABTestRouter`. Bucketing is
deterministic on `(tenant_id, experiment_name, bucket_key)`
where `bucket_key` defaults to the request's `tenant_id` when
the caller does not set one. The chosen arm is stamped on
`resp.Policy.ExperimentName` / `ExperimentArm` and persisted
in the `query_analytics` row so the results aggregator can
compute per-arm hit-rate / latency.

To stop an experiment: PATCH `enabled=false`. In-flight
requests already routed to a variant complete normally; new
requests fall back to the default config.

## 3. Pipeline health dashboard

**Endpoint:** `GET /v1/admin/pipeline/health`

Returns:
- queue depth per stage,
- retry-rate buckets (success / retry / dlq),
- last-N sync runs (status + duration + docs-processed),
- credential-validation outcomes.

Read in conjunction with:
- `GET /v1/admin/pipeline/retry-stats` (Round 6) for retry
  histogram broken down by stage and source.
- `GET /v1/admin/pipeline/backpressure` (Round 6) for priority
  buffer occupancy and drain rate.

Alarms on:
- Sustained queue depth > 10k → operator should investigate
  the slowest stage by latency and scale workers.
- Retry-rate above 25% on any stage → check connector tokens
  and downstream service health.

## 4. Exporting audit trails

**Endpoint:** `POST /v1/admin/audit/export`

Body fields: `since`, `until` (RFC3339), `format` (`csv` or
`jsonl`), and an optional `actions` filter array. Returns a
job ID; poll with `GET /v1/admin/jobs/{id}` for completion +
download URL.

## 5. Managing per-tenant latency budgets and cache TTL

**Latency budget:**
- `GET  /v1/admin/tenants/{tenant_id}/latency-budget`
- `PUT  /v1/admin/tenants/{tenant_id}/latency-budget`
  body: `{"max_latency_ms": 750}`

The retrieval handler reads this via
`Handler.SetLatencyBudgetLookup` and binds the budget to
`context.WithTimeout(req.Context, budget)`. Requests that
exceed the budget return 504 with `degraded=true`.

**Cache TTL:**
- `GET  /v1/admin/tenants/{tenant_id}/cache-config`
- `PUT  /v1/admin/tenants/{tenant_id}/cache-config`
  body: `{"ttl_ms": 60000}`

The lookup is consulted on every `cache.Set` so an updated
TTL takes effect on the *next* response written to Redis;
existing entries keep their original expiry.

## 6. Bulk source operations

**Endpoint:** `POST /v1/admin/sources/bulk`

Supports `op` ∈ {`connect`, `purge`, `reindex`} with a list
of source IDs. The handler returns a job ID; the per-source
outcomes are surfaced via `GET /v1/admin/jobs/{id}`. Failed
sources do not abort the batch; check the result map for
per-source error strings.

## 7. Reading the chunk quality report

**Endpoint:** `GET /v1/admin/chunks/quality`

Returns counts of:
- `low_information` chunks (very short / boilerplate),
- `near_duplicate` chunks dropped by the Stage 4 dedup
  (gated by `CONTEXT_ENGINE_DEDUP_ENABLED`),
- `empty` chunks (whitespace-only after sanitisation),
- per-source distribution of the above.

When the near-duplicate count is unusually high investigate
whether the connector is producing redundant chunk boundaries
(e.g. infinite-scroll page that re-emits the header on every
fetch) or whether the document genuinely contains repeated
content.

## 8. Credential health alerts

**Endpoint:** `GET /v1/admin/sources/{id}/credential-health`

The credential health worker (cmd/ingest/main.go) runs every
`CONTEXT_ENGINE_CREDENTIAL_HEALTH_INTERVAL` (default `1h`)
and calls `connector.Validate()` for every active source. The
last outcome is stored in `source_health.credential_valid`
+ `credential_checked_at` + `credential_error`. Failures emit
the audit event `source.credential_invalid` which fires any
matching notification subscriptions.

To force a re-check:

```bash
curl -X POST $API/v1/admin/sources/$ID/validate \
  -H "X-Tenant-ID: $TENANT"
```

When a credential is invalid the source's ingestion is
paused; the operator must rotate the token via the source's
admin page and re-enable the source. Notification
subscriptions for `source.credential_invalid` can be set up
via `POST /v1/admin/notifications/subscriptions`.

## 9. Notification delivery retries

Failed webhook deliveries are persisted in
`notification_delivery_log` with `next_retry_at` set when the
HTTP response is retryable (5xx or transport error). The
retry worker scans the table every minute, redelivers with
linear backoff, and dead-letters the row after
`DefaultMaxRetryAttempts` (5). Operators can inspect the log:

```bash
curl $API/v1/admin/notifications/delivery-log?limit=200 \
  -H "X-Tenant-ID: $TENANT"
```

## 10. Round-8 environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `CONTEXT_ENGINE_DEDUP_ENABLED` | `false` | Enable Stage 4 near-dup drop. |
| `CONTEXT_ENGINE_PRIORITY_ENABLED` | `false` | Route Submit through priority buffer. |
| `CONTEXT_ENGINE_CREDENTIAL_HEALTH_INTERVAL` | `1h` | Credential validation cadence. |
| `CONTEXT_ENGINE_NOTIFICATION_RETRY_INTERVAL` | `1m` | Retry worker tick interval. |
