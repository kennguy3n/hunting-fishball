# Google Workspace Directory runbook (`google_workspace`)

> Source: [`internal/connector/google_workspace/google_workspace.go`](../../internal/connector/google_workspace/google_workspace.go)
> · API base: `https://admin.googleapis.com/admin/directory/v1`
> · Delta cursor: `updatedMin=<RFC3339>` filter on
>   `/users?customer=my_customer` and `/groups`

## 1. Auth model

OAuth 2.0 access token issued to a service account with
domain-wide delegation, impersonating a super-admin. Required
scopes:

- `https://www.googleapis.com/auth/admin.directory.user.readonly`
- `https://www.googleapis.com/auth/admin.directory.group.readonly`

```json
{ "access_token": "ya29.A0..." }
```

Header: `Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation:

1. **Google Cloud Console → IAM & Admin → Service accounts**
   → select the directory-reader SA → **Keys** → **Add key →
   Create new key (JSON)**.
2. Update the secret-manager entry; the token refresher mints
   a fresh access token on the next cycle.
3. Verify: `GET /admin/directory/v1/users?customer=my_customer&maxResults=1`
   returns `200 OK`.
4. Delete the prior service-account key in the same blade.

Emergency rotation:

1. **Service accounts → Keys** → delete the leaked key.
2. Pause the affected source.
3. Audit **Admin → Reporting → Audit logs → Admin** for
   service-account API calls in the leak window.
4. If domain-wide delegation was compromised, revoke and
   re-issue the delegation grant in Admin → Security → API
   controls → Domain-wide delegation.

## 3. Quota / rate-limit incidents

Admin SDK enforces 2400 queries / minute / project. Throttle
signal:

- `429 Too Many Requests` with `Retry-After`.
- `403` with `userRateLimitExceeded` or `quotaExceeded` in the
  error body.
- The connector wraps all three to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 25 (well
  under the 40-rps sustained ceiling).
- Use `maxResults=500` (the Admin SDK maximum) to minimize
  request count.

## 4. Outage detection & recovery

Google publishes Workspace status at
https://www.google.com/appsstatus.

Common outage signatures:

- `503 Service Unavailable` from `/users` during Admin SDK
  regional failovers.
- `403` with `Quota exceeded` if a sister project on the same
  GCP org burned the directory quota.

Recovery:

1. Confirm via the Workspace status dashboard.
2. Delta poll resumes from the persisted `updatedMin`
   timestamp.
3. Suspended users (`suspended=true`) are emitted as
   `ChangeDeleted` so the downstream graph drops them
   automatically; un-suspending re-emits `ChangeUpserted`.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":{"code":401,"status":"UNAUTHENTICATED"}}` | Bearer expired | §2 routine rotation |
| `403` | `userRateLimitExceeded` | Per-user QPS exceeded | §3 |
| `403` | `quotaExceeded` | Project quota exhausted | §3 |
| `403` | `Not Authorized to access this resource/api` | Missing OAuth scope | Re-grant DWD |
| `404` | `notFound` | User/group out of scope | Iterator skips |
| `429` | `rateLimitExceeded` | Admin SDK throttle | §3 |
| `503` | n/a | Admin SDK regional failover | §4 |
