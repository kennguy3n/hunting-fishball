# Alerting & Dashboard Runbook

This runbook covers the Prometheus alerts in
[`deploy/alerts.yaml`](../../deploy/alerts.yaml) and the Grafana
dashboard in
[`deploy/grafana/context-engine-dashboard.json`](../../deploy/grafana/context-engine-dashboard.json).
Each alert below names the dashboard panel a triage-er should
open first.

## Severity conventions

| Severity   | Action                                              |
| ---------- | --------------------------------------------------- |
| `critical` | Page on-call. User-visible breakage or imminent SLO |
|            | violation. Owner must respond within the on-call    |
|            | response window.                                    |
| `warning`  | File a ticket. SLO at risk; service still up.       |
| `info`     | Dashboard-only. Trend monitoring; no paging.        |

## Alert → dashboard panel index

| Alert                | Severity   | Dashboard panel ID(s) |
| -------------------- | ---------- | --------------------- |
| `IngestionLagHigh`   | critical   | 3 (Kafka Consumer Lag)|
| `IngestionLagSustained` | critical| 3                     |
| `DLQRateHigh`        | warning    | 4 (DLQ Depth)         |
| `RetrievalP95High`   | warning    | 2 (Retrieval p95)     |
| `RetrievalP99High`   | critical   | 2                     |
| `PipelineStageSlow`  | warning    | 1 (Pipeline Stage p95)|
| `SourceUnhealthy`    | warning    | 5 (Connector Health)  |

Round-5 panels (no alerts yet — info-only):

| Round-5 metric                          | Panel ID |
| --------------------------------------- | -------- |
| `context_engine_token_refreshes_total`  | 7        |
| `context_engine_index_auto_reindexes_total` | 8    |

## Per-alert triage

### IngestionLagHigh / IngestionLagSustained

**Symptom.** A Kafka consumer group is more than 10 000 messages
behind on a topic for >5 minutes (`High`) or >50 000 for >30
minutes (`Sustained`).

**Triage (panel 3):**

1. Identify the topic + group from the alert labels.
2. Check `panel 1` (Pipeline Stage p95) for an upstream slow
   stage — a parse / embed slowdown is the usual driver.
3. Check the parse + embed sidecar pods are healthy
   (`docker compose ps` locally, or the cluster's pod health).
4. If a sidecar is degraded, scale the ingest worker
   (`docker compose up -d --scale ingest=N` locally; in
   production, bump the `ingest` Deployment's replicas).

### DLQRateHigh

**Symptom.** DLQ ingest rate exceeds 1 message/second over a
5-minute window.

**Triage (panel 4):**

1. Open the DLQ list endpoint
   (`GET /v1/admin/dlq?limit=50`) and group failures by
   `original_topic` to pinpoint the failing pipeline stage.
2. Inspect the most recent failure's stored payload and error
   for the root cause.
3. After fixing the root cause, batch-replay the affected rows
   via `POST /v1/admin/dlq/replay` (Round-5 Task 11) with the
   appropriate filters.

### RetrievalP95High / RetrievalP99High

**Symptom.** End-to-end retrieval p95/p99 latency above the SLO
budget.

**Triage (panel 2):**

1. Identify the slowest backend in panel 2.
2. If it's `vector`, check Qdrant health
   (`GET /v1/admin/index/health`).
3. If it's `lexical`, check Bleve / OpenSearch health.
4. If it's `graph`, check FalkorDB query duration in the same
   panel — a runaway graph traversal is the most common cause.
5. As a containment step, the watchdog (Task 18) auto-flips the
   tenant's `unhealthy` backend to read-only mode and triggers
   a reindex; see `index.auto_reindex_triggered` audit events.

### PipelineStageSlow

**Symptom.** A pipeline stage's p95 duration exceeds 30s for >5
minutes.

**Triage (panel 1):**

1. The stage name in the alert label points to the slow stage.
2. `parse` slowness usually means a docling pod is degraded.
3. `embed` slowness usually means the embedding service is
   over-loaded — scale up.
4. `store` slowness usually means a downstream backend is
   degraded — see `RetrievalP95High` triage.

### SourceUnhealthy

**Symptom.** A `(tenant, source)` pair has accumulated >25
errors over a 10-minute window.

**Triage (panel 5):**

1. Open `GET /v1/admin/sources/:id/health` for the affected
   source.
2. Check the credential expiry monitor's gauge
   (`context_engine_credentials_expiring_days`) — an expired or
   soon-to-expire credential is the most common driver.
3. If the credential is healthy, check the connector's recent
   webhook activity in the audit log (`webhook.failed`).
4. The credential rotation worker (Round-5 Task 14) emits
   `source.credential_expiring` audit events 7 days before
   expiry; if you're seeing one of those, kick off rotation.

## Dashboard provisioning

The dashboard is provisioned via the standard Grafana sidecar
pattern. To install:

```bash
# Copy the dashboard JSON into the provisioning ConfigMap.
kubectl create configmap grafana-context-engine \
  --from-file=deploy/grafana/context-engine-dashboard.json \
  -n monitoring \
  --dry-run=client -o yaml | kubectl apply -f -
```

The dashboard uses `${tenant}` and `${backend}` template
variables, both populated from Prometheus `label_values` queries.
For local development, `make compose-up` brings up Prometheus
and Grafana; the dashboard is mounted automatically.
