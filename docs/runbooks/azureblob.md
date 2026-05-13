# Azure Blob Storage runbook (`azure_blob`)

> Source: [`internal/connector/azure_blob/azure_blob.go`](../../internal/connector/azure_blob/azure_blob.go)
> · API base: `https://<account>.blob.core.windows.net`
> · Delta cursor: RFC 1123 timestamp compared against
>   `x-ms-blob-last-modified` (sent as `If-Modified-Since`).

## 1. Auth model

The connector supports either a SAS token (Service-level
Shared Access Signature) or a Shared Key (Storage account
key) for HMAC-SHA256 signing.

```json
{
  "account":    "acmestorage",
  "container":  "context-engine",
  "sas_token":  "?sv=2024-...",
  "shared_key": ""
}
```

When `shared_key` is set, the connector signs each request
per Azure's "Construct the canonicalized headers string"
specification with `Authorization: SharedKey <account>:<sig>`.
When `sas_token` is set, the SAS query string is appended
verbatim and no `Authorization` header is sent.

## 2. Credential rotation

Routine rotation (Shared Key):

1. **Azure portal → Storage account → Access keys** →
   regenerate `key2`.
2. Update the secret-manager record with `key2`.
3. Verify: HEAD on the container returns 200.
4. Once the change has propagated to every runner,
   regenerate `key1` so any leaked copy is invalidated.

Routine rotation (SAS):

1. **Storage account → Shared access signature** → create a
   new SAS scoped to the target container with `Read, List`
   permissions and a future expiry.
2. Update the secret-manager record.
3. The previous SAS automatically expires at its `se=` time
   stamp.

Emergency rotation:

1. **Storage account → Access keys** → regenerate both
   `key1` and `key2` immediately to invalidate any leaked
   key.
2. Revoke leaked SAS via **Stored access policies** if a
   policy was used; otherwise the SAS remains valid until
   its expiry (rotate `key` to invalidate).

## 3. Quota / rate-limit incidents

Azure Blob enforces account-level scalability targets and
per-request throttling. Throttle signal:

- `503 ServerBusy` with `x-ms-error-code: ServerBusy`.
- `500 InternalError` with `x-ms-error-code: OperationTimedOut`
  during high load.
- The connector wraps `429` as
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go);
  the adaptive limiter additionally honors `503 ServerBusy`.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 8.
- Use the `marker` continuation token rather than `prefix`
  scans to avoid full-container enumeration on every poll.

## 4. Outage detection & recovery

Azure publishes status at https://status.azure.com.

Common outage signatures:

- `503 ServerBusy` during region-level throttling.
- DNS resolution failures on
  `<account>.blob.core.windows.net` during regional failover.

Recovery:

1. Confirm via the Azure status page.
2. Resume from the persisted `Last-Modified` cursor — Azure
   guarantees monotonic `x-ms-blob-last-modified` per blob.
3. For deleted blobs, the listing simply omits them; a
   periodic full reconcile emits the tombstones.

## 5. Common error codes

| Upstream | `x-ms-error-code` | Meaning | Action |
|----------|---------------------|---------|--------|
| `401` | `AuthenticationFailed` | Key rotated / SAS expired | §2 |
| `403` | `AuthorizationPermissionMismatch` | SAS scoped without `Read`/`List` | Reissue SAS |
| `404` | `BlobNotFound` | Blob deleted | Iterator skips |
| `429` | `OperationTimedOut` | Throttled | §3 |
| `503` | `ServerBusy` | Account-level throttling | §3 + back-off |
