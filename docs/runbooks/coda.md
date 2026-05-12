# Coda runbook (`coda`)

> Source: [`internal/connector/coda/coda.go`](../../internal/connector/coda/coda.go)
> · API base: `https://coda.io/apis/v1`
> · Delta cursor: `sortBy=updatedAt&direction=DESC` walk
>   against `/docs`, stopping at the high-water `updatedAt`.

## 1. Auth model

Coda issues personal API tokens scoped to the issuing user.
The token grants access to every doc the issuing user can
see — there is no finer-grained scope.

```json
{ "access_token": "abc..." }
```

Header: `Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation:

1. **Coda → Account settings → API → Generate new token**
   (issued tokens are user-scoped, not workspace-scoped — use
   a dedicated integration user, not a real employee).
2. Update the secret-manager record.
3. Verify: `GET /whoami` returns the integration user.
4. **Account settings → API → Tokens** → revoke the previous
   token.

Emergency rotation:

1. **Account settings → API → Tokens** → revoke the leaked
   token immediately.
2. Pause every Coda source.
3. Audit **Coda → Admin → Activity log** (Enterprise plans
   only) for `api_token.use` events in the leak window. On
   non-Enterprise plans, open a support ticket for the API
   access history.
4. If the integration user has elevated permissions on
   sensitive docs (e.g. HR docs), rotate the user's password
   and revoke its membership from those docs before
   re-issuing a token.

## 3. Quota / rate-limit incidents

Coda enforces a sliding-window rate limit (commonly 10 rps
read, lower for write). Throttle signal:

- `429 Too Many Requests` with `Retry-After` (seconds).
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 6.
- Use `limit=100` (Coda's maximum page size) and rely on
  `pageToken` pagination to minimize request count.

## 4. Outage detection & recovery

Coda publishes status at https://status.coda.io.

Common outage signatures:

- `503 Service Unavailable` from `/docs` during regional
  failover.
- `502 Bad Gateway` from the CDN if Coda's API gateway is
  partially deployed.

Recovery:

1. Confirm via the status page.
2. Delta poll resumes from the persisted `updatedAt`
   timestamp. Coda's `updatedAt` is monotonically
   non-decreasing per doc; pages from `sortBy=updatedAt&
   direction=DESC` stop once they reach the cursor.
3. Docs deleted from Coda disappear from the `/docs` listing
   silently — schedule a periodic full reconciliation to
   emit `ChangeDeleted` tombstones.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"statusCode":401,"statusMessage":"Unauthorized"}` | Token revoked / expired | §2 routine rotation |
| `403` | `{"statusCode":403,"statusMessage":"Forbidden"}` | Token-user lost access to the doc | Re-share doc with integration user |
| `404` | `{"statusCode":404,"statusMessage":"NotFound"}` | Doc deleted | Iterator skips |
| `429` | `{"statusCode":429}` | Rate limit | §3 |
| `503` | n/a | Coda outage | §4 |
