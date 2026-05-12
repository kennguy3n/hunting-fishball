# Workday HR runbook (`workday`)

> Source: [`internal/connector/workday/workday.go`](../../internal/connector/workday/workday.go)
> · API base: `https://<tenant>.workday.com/ccx/api/v1/<tenant>`
> · Delta cursor: `Updated_From=<RFC3339>` filter on
>   `/workers?DESC` (lastModifiedDateTime)

## 1. Auth model

OAuth 2.0 Bearer issued from a Workday Integration System User
(ISU) configured with the **HR Worker Data: View** domain.

```json
{
  "access_token": "tok-...",
  "tenant_id": "acme",
  "tenant_url": "https://wd5-impl.workday.com/ccx/api/v1/acme",
  "source_id": "wd-prod"
}
```

Header: `Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation:

1. **Workday → Maintain Refresh Tokens** → revoke the prior
   ISU refresh token.
2. Re-authenticate the ISU through the OAuth consent flow to
   mint a fresh refresh token; encrypt and store in the
   secret manager.
3. Verify: `GET /workers?limit=1` returns `200 OK` with one
   `data[]` row.
4. Audit **System → Tenant security events** for `OAuth
   Refresh Token` activity to confirm only the new token is
   in use.

Emergency rotation:

1. **Maintain Refresh Tokens** → revoke ALL refresh tokens
   associated with the ISU.
2. Pause every Workday source.
3. Run **Tenant security events** for `View: Worker` requests
   in the leak window to scope the blast radius.
4. Rotate the underlying ISU password (Workday → Maintain
   Users → reset password) before re-authenticating.

## 3. Quota / rate-limit incidents

Workday enforces a per-tenant concurrency cap on REST. The
exact rate varies by tenant tier; throttle signals:

- `429 Too Many Requests` with `Retry-After` (seconds).
- `503 Service Unavailable` during tenant capacity exhaustion.
- Both are wrapped to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 3 (most
  tenants tolerate 5 rps sustained; we leave 40% headroom).
- Use `limit=100` (Workday's default REST page size).

## 4. Outage detection & recovery

Workday publishes status at https://www.workday.com/en-us/company/security/trust-site.html
(Trust → System Status). Tenant-specific status is also visible
in the Workday community → **My Workday → Service Health**.

Common outage signatures:

- `503` from `/workers` during the monthly Workday tenant
  maintenance window (typically Saturday 02:00–14:00 PT).
- `403` `User has no access to this URI` if the ISU's domain
  security policy was inadvertently changed.

Recovery:

1. Confirm via Workday status / community.
2. Delta poll resumes from the persisted `lastModifiedDateTime`
   cursor.
3. Terminated workers (`active=false`, `terminationDate≠null`)
   are emitted as `ChangeDeleted` so the directory drops them
   automatically; rehires re-emit `ChangeUpserted`.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":"invalid_token"}` | Bearer expired/revoked | §2 routine rotation |
| `403` | `User has no access to this URI` | Domain security misconfigured | Grant ISU **HR Worker Data: View** |
| `404` | `Resource not found` | Worker terminated + purged | Iterator skips |
| `429` | `Rate limit exceeded` | Workday tenant throttle | §3 |
| `503` | n/a | Maintenance window | §4 |
