# Pipedrive runbook (`pipedrive`)

> Source: [`internal/connector/pipedrive/pipedrive.go`](../../internal/connector/pipedrive/pipedrive.go)
> · API base: `https://<company-domain>.pipedrive.com/api/v1`
> · Webhook: Pipedrive webhooks (planned for a later round)
> · Delta cursor: `since_timestamp=<unix>` against `/recents`

## 1. Auth model

Per-user API token appended to the query string (NO header).

```json
{ "api_token": "abc...", "company_domain": "acme" }
```

Each request appends `?api_token=...` (we wire it via
`url.Values`).

Required scopes: the token inherits the user's permissions; the
user must have **deals.read**, **persons.read**, and
**activities.read** at minimum.

## 2. Credential rotation

Routine rotation:

1. Pipedrive → **Avatar** → **Personal preferences** → **API**
   → **Generate new API token**.
2. PATCH the source credential via the admin portal.
3. Verify: `GET /api/v1/users/me?api_token=...` returns the user.

Emergency rotation:

1. Same path as routine — clicking **Generate** invalidates the
   previous value.
2. Pause the source.
3. Audit Pipedrive's **Account settings** → **Audit Log** for
   actions taken with the leaked token.

## 3. Quota / rate-limit incidents

Pipedrive enforces a token-bucket: 100 tokens/2s per account on
Essential, scaling up to 480/2s on Enterprise. Bursts above the
bucket capacity return 429.

Throttle signal:

- `429 Too Many Requests` with
  `{"success":false,"error":"Rate limit reached"}`.
- `X-RateLimit-Remaining` header drops to 0.

Wraps to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 5 (matches
  Essential ≈ 50/s halved).
- The connector enumerates `deals`, `persons`, and `activities` as
  separate namespaces; the steady-state path uses `/recents` (one
  call across all object types) to amortise the per-call cost.

## 4. Outage detection & recovery

Pipedrive publishes status at https://status.pipedrive.com.

Common outage signatures:

- `502 Bad Gateway` from `*.pipedrive.com/api/v1` during deploys.
- `500 Internal Server Error` from `/recents` when the
  `since_timestamp` window is too wide — narrow it.

Recovery:

1. Confirm via the status page.
2. Delta poll resumes from the last persisted unix timestamp.
3. For prolonged outages, `/recents` returns a 24h-bounded slice
   — for outages longer than 24h, ListDocuments must backfill.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"success":false,"error":"Unauthorized"}` | Token revoked | §2 routine rotation |
| `403` | `{"success":false,"error":"Forbidden"}` | Permission missing | Grant the user the required scope |
| `404` | `{"success":false,"error":"Deal not found"}` | Object deleted | Iterator skips |
| `429` | `{"success":false,"error":"Rate limit reached"}` | Burst exceeded | §3 |
| `500` | n/a | Upstream incident | §4 |
| `502` | n/a | Upstream deploy | Iterator retries; resumes from cursor |
