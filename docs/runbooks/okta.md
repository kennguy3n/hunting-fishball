# Okta runbook (`okta`)

> Source: [`internal/connector/okta/okta.go`](../../internal/connector/okta/okta.go)
> · API base: `https://<org>.okta.com/api/v1`
> · Webhook: Okta event hooks (planned for a later round)
> · Delta cursor: `filter=lastUpdated gt "<RFC3339>"` against
>   `/users` and `/groups`

## 1. Auth model

Okta API token (NO `Bearer` prefix — uses `SSWS` scheme).

```json
{ "api_token": "00...XYZ", "org_url": "https://acme.okta.com" }
```

Header: `Authorization: SSWS 00...XYZ`.

Required role: **Read-only Administrator** at minimum
(`okta.users.read` + `okta.groups.read` admin role bundle).

## 2. Credential rotation

Routine rotation:

1. Okta Admin Console → **Security** → **API** → **Tokens** →
   **Create Token** (issues a new SSWS token).
2. PATCH the source credential via the admin portal.
3. Verify: `GET /api/v1/users/me` returns the admin user.
4. **Security** → **API** → **Tokens** → revoke the previous
   token.

Emergency rotation:

1. **Security** → **API** → **Tokens** → revoke the leaked
   token immediately.
2. Pause the source.
3. Issue a fresh token from a different admin account (Okta
   tokens are tied to the issuing admin's lifecycle).
4. Audit **Reports** → **System Log** filtered to
   `eventType eq "system.api_token.use"` for actions taken with
   the leaked token.

## 3. Quota / rate-limit incidents

Okta enforces per-endpoint org-wide concurrent rate limits
(default 600 req/min for `/users`, lower for `/groups`).

Throttle signal:

- `429 Too Many Requests` with `X-Rate-Limit-Reset` (unix
  seconds) and `X-Rate-Limit-Remaining`.
- Okta also responds with `503 Service Unavailable` when the
  concurrent-request quota is exhausted — the connector wraps
  this to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 5 (matches
  600/min halved).
- Reduce per-page `limit=200` (the Okta default) to spread the
  bucket if the org is dense.

## 4. Outage detection & recovery

Okta publishes status at https://status.okta.com.

Common outage signatures:

- `503 Service Unavailable` from `/users` during regional
  Okta cell failovers.
- `403 Forbidden` from `/users` with `E0000007` when the org
  is in **read-only maintenance** during an Okta upgrade.

Recovery:

1. Confirm via the status page.
2. Delta poll resumes from the last persisted `lastUpdated`
   timestamp.
3. Deprovisioned users are emitted as `ChangeDeleted` so the
   downstream index converges automatically.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"errorCode":"E0000011","errorSummary":"Invalid token provided"}` | Token revoked | §2 routine rotation |
| `403` | `{"errorCode":"E0000006","errorSummary":"You do not have permission..."}` | Admin role missing | Grant **Read-only Administrator** |
| `403` | `{"errorCode":"E0000007","errorSummary":"Not found"}` | User/group out of scope | Iterator skips |
| `429` | `{"errorCode":"E0000047","errorSummary":"API call exceeded rate limit"}` | Rate limit | §3 |
| `503` | n/a | Concurrent quota / cell failover | §3 / §4 |
