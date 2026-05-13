# Freshdesk runbook (`freshdesk`)

> Source: [`internal/connector/freshdesk/freshdesk.go`](../../internal/connector/freshdesk/freshdesk.go)
> · API base: `https://<domain>.freshdesk.com/api/v2`
> · Delta cursor: `updated_since=<ISO8601>` watermark on
>   `/api/v2/tickets`

## 1. Auth model

Freshdesk REST API v2 authenticates via HTTP basic auth where
**the API key is the username** and a literal `"X"` is the
password — same pattern as BambooHR.

```json
{
  "domain": "acme",
  "api_key": "..."
}
```

Header: `Authorization: Basic base64(<api_key>:X)`.

## 2. Credential rotation

Routine rotation:

1. **Admin → Account → Profile Settings** of the integration
   agent → **API Key** → **Reset**.
2. Update the secret-manager record.
3. Verify with `GET /api/v2/agents/me` — must return `200 OK`.
4. The old key is immediately invalidated by **Reset**; no
   second-step revocation is required.

Emergency rotation:

1. Disable the integration agent (**Admin → Agents** →
   **Deactivate**) — this invalidates the key.
2. Pause the source.
3. Use the **Audit Log** (Admin → Account → Audit Log) to
   enumerate any actions the key took during the compromise
   window.
4. Re-activate, **Reset API Key**, and right-scope the
   integration agent's groups/roles before re-enabling.

## 3. Quota / rate-limit incidents

Freshdesk enforces a plan-tiered per-minute quota (Estate plan
~700/min, Pro ~400/min). Throttle signal:

- `429 Too Many Requests` with a `Retry-After` header.
- `X-RateLimit-Remaining` header on every successful response.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go)
  and the adaptive limiter honors `Retry-After`.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 4 (Pro
  plan).
- `per_page=100` is the max — page-based pagination via
  `page=<n>` (1-indexed). Beyond page 300 Freshdesk returns
  empty.

## 4. Outage detection & recovery

Freshdesk publishes status at https://status.freshworks.com
(rolled up across all Freshworks products). Pod failovers are
regional (US, EU, IND, AUS).

Common outage signatures:

- `503 Service Unavailable` during pod failover.
- `502 Bad Gateway` from the CDN during certificate rotation.
- `408 Request Timeout` for ticket attachments larger than
  ~15MB — the connector skips these and emits a metric.

Recovery:

1. Confirm via https://status.freshworks.com.
2. Delta resumes from the persisted `updated_at` ISO8601
   timestamp. Freshdesk guarantees the `updated_since` filter
   is exactly-once if the cursor monotonically advances.
3. Tickets merged during the outage window are surfaced as
   `ChangeUpserted` on the next delta — the merged-into ticket
   carries the merged content; the merged-from is marked
   `status=closed` and skipped on subsequent ticks.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Authentication failed` | Key reset or agent deactivated | §2 |
| `403` | `Access denied` | Agent lacks group membership for the ticket | Re-scope groups |
| `404` | `Resource not found` | Ticket hard-deleted (90-day retention) | Iterator skips |
| `429` | `Rate limit exceeded` | Per-account quota | §3 |
| `500` | `Internal Server Error` | Freshdesk side; usually transient | Retry with backoff |
| `503` | `Service Unavailable` | Pod failover | §4 |
