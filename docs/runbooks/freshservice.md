# Freshservice runbook (`freshservice`)

> Source: [`internal/connector/freshservice/freshservice.go`](../../internal/connector/freshservice/freshservice.go)
> · API base: `https://<domain>.freshservice.com/api/v2`
> · Delta cursor: `updated_since=<ISO8601>` watermark on
>   `/api/v2/tickets`

## 1. Auth model

Freshservice REST API v2 authenticates via HTTP basic auth
where **the API key is the username** and a literal `"X"` is
the password — same pattern as Freshdesk (the two share the
Freshworks identity backbone).

```json
{
  "domain": "acme",
  "api_key": "..."
}
```

Header: `Authorization: Basic base64(<api_key>:X)`.

## 2. Credential rotation

Routine rotation:

1. **Admin → Account → Agent Profile** of the integration
   agent → **API Key** → **Reset**.
2. Update the secret-manager record.
3. Verify with `GET /api/v2/agents/me` — must return `200 OK`.
4. The old key is invalidated by **Reset** immediately.

Emergency rotation:

1. Deactivate the integration agent (Admin → Agents →
   **Deactivate**) — invalidates the key in one step.
2. Pause the source.
3. Pull the audit log (Admin → Account → Audit Log) and
   enumerate any tickets the key touched during the
   compromise window.
4. Re-activate, **Reset API Key**, scope the agent to the
   minimum required groups before re-binding.

## 3. Quota / rate-limit incidents

Freshservice enforces a plan-tiered per-minute quota
(Enterprise ~280/min, Pro ~160/min). Throttle signal:

- `429 Too Many Requests` with a `Retry-After` header.
- `X-RateLimit-Total` / `X-RateLimit-Remaining` /
  `X-RateLimit-Used-CurrentRequest` headers on every
  response.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go)
  and the adaptive limiter honours `Retry-After`.

Knobs:

- `per_page=100` is the max — page-based pagination via
  `page=<n>` (1-indexed).
- The connector keys page-end detection on
  `Link: rel="next"` (same fix the Round-22 Freshdesk patch
  introduced) so 100-row exact pages are detected correctly.

## 4. Outage detection & recovery

Freshservice rolls up under https://status.freshworks.com
alongside Freshdesk. Pod failovers are regional (US, EU,
IND, AUS).

Common outage signatures:

- `503 Service Unavailable` during pod failover.
- `502 Bad Gateway` from the CDN during certificate
  rotation.
- `504 Gateway Timeout` on attachment downloads larger
  than ~20MB — the connector skips and emits a metric.

Recovery:

1. Confirm via https://status.freshworks.com.
2. Delta resumes from the persisted `updated_at` cursor.
3. Tickets merged during the outage window are surfaced as
   `ChangeUpserted` on the next delta.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Authentication failed` | Key reset or agent deactivated | §2 |
| `403` | `Access denied` | Agent lacks group membership for the ticket | Re-scope groups |
| `404` | `Resource not found` | Ticket hard-deleted | Iterator skips |
| `429` | `Rate limit exceeded` | Per-account quota | §3 |
| `500` | `Internal Server Error` | Freshservice side; usually transient | Retry with backoff |
| `503` | `Service Unavailable` | Pod failover | §4 |
