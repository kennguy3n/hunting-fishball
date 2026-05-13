# Google Cloud Storage runbook (`gcs`)

> Source: [`internal/connector/gcs/gcs.go`](../../internal/connector/gcs/gcs.go)
> · API base: `https://storage.googleapis.com/storage/v1`
> · Delta cursor: RFC 3339 timestamp compared against each
>   object's `updated` field.

## 1. Auth model

The connector consumes a pre-resolved OAuth 2.0 access
token (Bearer); the platform's token-refresh worker handles
renewal against the service-account private key.

```json
{ "access_token": "ya29...", "bucket": "ctx-engine-prod" }
```

Header: `Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation (service account):

1. **GCP console → IAM & Admin → Service accounts → Keys** →
   create a new JSON key for `context-engine@…`.
2. Update the secret-manager record with the new key.
3. Verify: `GET /b/<bucket>` returns 200.
4. **Service accounts → Keys** → delete the prior key.

Routine rotation (workload identity):

1. Update the Kubernetes service-account annotation
   (`iam.gke.io/gcp-service-account`) if pointing at a new
   GSA.
2. No on-disk credential exists; verify by triggering a sync.

Emergency rotation:

1. **IAM & Admin → Service accounts → Keys** → delete the
   leaked key.
2. Pause every GCS source.
3. Review **GCP audit logs → Data Access** for the leaked
   key's `principalEmail` in the leak window.

## 3. Quota / rate-limit incidents

GCS enforces per-project + per-bucket QPS limits. Throttle
signal:

- `429 Too Many Requests` with `Retry-After`.
- `503 Service Unavailable` during sustained throttling.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 10.
- The `pageSize` ceiling is 1000 — use the maximum to keep
  request count low.

## 4. Outage detection & recovery

GCP publishes status at https://status.cloud.google.com.

Common outage signatures:

- `503 Service Unavailable` during multi-region writes.
- Elevated latency on `storage.googleapis.com` correlated to
  the Status Dashboard.

Recovery:

1. Confirm via the status page.
2. Resume from the persisted `updated` cursor — GCS
   guarantees monotonically non-decreasing `updated` per
   object generation.
3. Versioned buckets: schedule a periodic full reconcile to
   pick up tombstones (`metageneration` jumps without an
   `updated` change).

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":{"code":401,"message":"Invalid Credentials"}}` | Token expired | Refresh trigger |
| `403` | `{"error":{"code":403,"message":"… does not have storage.objects.list"}}` | Missing IAM role | Grant `roles/storage.objectViewer` |
| `404` | `{"error":{"code":404}}` | Bucket or object deleted | Iterator skips |
| `429` | `{"error":{"code":429}}` | QPS exceeded | §3 |
| `503` | `{"error":{"code":503}}` | Region-level throttling | §3 + back-off |
