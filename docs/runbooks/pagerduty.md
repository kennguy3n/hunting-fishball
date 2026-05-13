# PagerDuty runbook (`pagerduty`)

> Source: [`internal/connector/pagerduty/pagerduty.go`](../../internal/connector/pagerduty/pagerduty.go)
> · API base: `https://api.pagerduty.com`
> · Delta cursor: `since=<ISO8601>` watermark on
>   `/incidents`

## 1. Auth model

PagerDuty supports two token types and the connector handles
both transparently:

- **User token** — scoped to a single REST API user. The
  connector verifies with `GET /users/me`.
- **General Access REST API key** (service-account style) —
  scoped to the account. `GET /users/me` returns 401 here,
  so the connector falls back to `GET /abilities` which is
  granted to both token types.

```json
{
  "api_key": "..."
}
```

Header: `Authorization: Token token=<api_key>` (note the
`Token token=` prefix — *not* a bare bearer).

## 2. Credential rotation

Routine rotation:

1. **Integrations → API Access Keys** in the PagerDuty admin
   panel → **Create New API Key**.
2. Mark the new key with the same scopes (read-only is
   sufficient for ingest).
3. Update the secret-manager record.
4. Verify with `GET /abilities` (or `GET /users/me` for user
   tokens) — must return `200 OK`.
5. **Delete** the prior key in the same panel once the
   replacement is live.

Emergency rotation:

1. Delete the compromised key via the API Access Keys panel
   — invalidation is immediate.
2. Pause the source.
3. Pull the PagerDuty audit log
   (`/audit/records?root_resource_types=api_key`) for the
   compromise window.
4. Generate a fresh key with minimum scopes (read-only,
   service-account holder) and re-bind.

## 3. Quota / rate-limit incidents

PagerDuty enforces a flat 960 requests/minute for the REST
API. Throttle signal:

- `429 Too Many Requests`.
- `X-RateLimit-Limit` and `X-RateLimit-Remaining` headers on
  every response.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- The default `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` of 8
  is well within the 960/min ceiling and tolerates burst
  pagination on the `/incidents` walk.
- Pagination uses cursor-style `offset` + `limit=100` and
  honours the `more` flag returned in each page.

## 4. Outage detection & recovery

PagerDuty status: https://status.pagerduty.com. PagerDuty
runs hot-hot across two regions; incidents are rare but
the API can degrade independently of the dispatcher.

Common outage signatures:

- `502 Bad Gateway` during edge maintenance.
- `503 Service Unavailable` from the REST API while the
  control plane is being upgraded.
- `504 Gateway Timeout` on `/incidents` with very wide
  `since` windows — the connector caps the window at 24h
  per page to avoid this.

Recovery:

1. Confirm via https://status.pagerduty.com.
2. Delta resumes from the persisted ISO8601 cursor —
   PagerDuty's `since` filter is exactly-once for the
   monotonic `last_status_change_at` field the connector
   tracks.
3. Incidents resolved during the outage are surfaced as
   `ChangeUpserted` on the next delta.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Unauthorized` | Key deleted or scope revoked | §2 |
| `403` | `Forbidden` | Token holder lacks `incidents:read` scope | Re-mint with broader scope |
| `404` | `Not found` | Incident hard-deleted (rare) | Iterator skips |
| `429` | `Rate limit exceeded` | Per-account quota | §3 |
| `500` | `Internal Server Error` | PagerDuty side; transient | Retry with backoff |
| `503` | `Service Unavailable` | Control plane upgrade | §4 |
