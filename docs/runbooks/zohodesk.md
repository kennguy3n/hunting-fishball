# Zoho Desk runbook (`zoho_desk`)

> Source: [`internal/connector/zoho_desk/zoho_desk.go`](../../internal/connector/zoho_desk/zoho_desk.go)
> · API base: `https://desk.zoho.com`
> · Delta cursor: `modifiedTimeRange=<since>,<until>`
>   watermark on `/api/v1/tickets`

## 1. Auth model

Zoho Desk authenticates via a Zoho OAuth 2.0 access token
plus the organisation ID. Tokens are short-lived (~1 hour)
and minted from a refresh token held in the secret manager.

```json
{
  "access_token": "...",
  "org_id": "12345"
}
```

Headers:

- `Authorization: Zoho-oauthtoken <access_token>` (note the
  `Zoho-oauthtoken` scheme — *not* `Bearer`).
- `orgId: <org_id>`.

## 2. Credential rotation

Routine rotation:

1. **Zoho API Console → Self Client / Server-Based**
   integration → **Generate Code**.
2. Exchange the auth code for `access_token` +
   `refresh_token` via
   `POST https://accounts.zoho.com/oauth/v2/token`.
3. Persist the new `refresh_token` to the secret manager and
   rebuild the live `access_token`.
4. Verify with `GET /api/v1/myinfo` (requires `Desk.basic.READ`)
   — must return `200 OK`.

Emergency rotation:

1. Revoke the refresh token at
   https://accounts.zoho.com/u/h#sessions/userauthtoken — this
   invalidates every access token derived from it.
2. Pause the source.
3. Pull the Zoho Desk audit log
   (Setup → Data Administration → Audit Log) for the
   compromise window.
4. Mint a fresh refresh token in the API Console with the
   minimum required scopes (`Desk.tickets.READ`,
   `Desk.basic.READ`) and re-bind.

## 3. Quota / rate-limit incidents

Zoho Desk enforces a daily API credit budget and a sliding
per-minute concurrency cap. Throttle signal:

- `429 Too Many Requests` with `Retry-After` (in seconds).
- `X-RATELIMIT-INFO` header reporting the remaining credits
  on every response.
- `400` with body `errorCode=API_LIMIT_EXCEEDED` for the
  daily credit cap. The connector treats this as
  rate-limited.

Knobs:

- Pagination uses `from=<offset>` + `limit=<100>` with the
  hard ceiling of 100/page.
- The connector accepts both `200 OK` with a `data` array
  and `204 No Content` (Zoho returns 204 when the page is
  empty) — the iterator stops cleanly on either.

## 4. Outage detection & recovery

Zoho status: https://status.zoho.com (rolled up across all
Zoho products; filter to Desk). Zoho's data residency is
region-bound — outages tend to affect one DC (US, EU, IND,
AUS, CN, JP) at a time.

Common outage signatures:

- `503 Service Unavailable` during DC maintenance.
- `502 Bad Gateway` from the Zoho edge during release.
- Spurious `204 No Content` on a window that should have
  data — Zoho's read replicas occasionally lag. The
  connector treats 204 as "no new data" and the cursor
  does NOT advance, so the next tick re-reads the window.

Recovery:

1. Confirm via https://status.zoho.com.
2. Delta resumes from the persisted ISO8601 cursor —
   `modifiedTimeRange` filtering is exactly-once when
   the cursor monotonically advances.
3. Tickets reassigned across departments during the
   outage window surface as upserts; ticket renames don't
   change the ID.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `400` | `INVALID_OAUTHTOKEN` | Access token expired | Refresh from refresh_token |
| `400` | `API_LIMIT_EXCEEDED` | Daily credit cap hit | §3 |
| `401` | `Unauthorized` | Token revoked / refresh token rotated | §2 |
| `403` | `Forbidden` | Scope missing `Desk.tickets.READ` | Re-mint with broader scope |
| `404` | `Not found` | Ticket hard-deleted | Iterator skips |
| `429` | `Too Many Requests` | Per-minute concurrency cap | §3 |
| `503` | `Service Unavailable` | DC maintenance | §4 |
