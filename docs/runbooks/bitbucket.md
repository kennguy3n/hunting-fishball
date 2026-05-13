# Bitbucket runbook (`bitbucket`)

> Source: [`internal/connector/bitbucket/bitbucket.go`](../../internal/connector/bitbucket/bitbucket.go)
> · API base: `https://api.bitbucket.org`
> · Delta cursor: `q=updated_on>"<ISO8601>"` filter on
>   `/2.0/repositories/<ws>/<repo>/pullrequests`

## 1. Auth model

Bitbucket Cloud REST API v2 authenticates via HTTP basic auth
with a **username + app password** (recommended for service
accounts) or an OAuth consumer access token. App passwords
are scoped to a fixed set of permissions (read PRs, read
issues, read code, etc.) at creation time and cannot be
re-scoped.

```json
{
  "username":     "integration",
  "app_password": "...",
  "workspace":    "acme",
  "repo":         "context-engine"
}
```

Header: `Authorization: Basic base64(<username>:<app_password>)`.

## 2. Credential rotation

Routine rotation:

1. **Personal settings → App passwords → Create app password**
   with the same permission set (Read pull requests, Read
   repository, Read issues as needed).
2. Update the secret-manager record.
3. Verify with
   `GET /2.0/repositories/<ws>/<repo>` — must return `200 OK`.
4. **App passwords → Revoke** the previous password.

Emergency rotation:

1. **App passwords → Revoke** the leaked password immediately.
2. Pause the source.
3. Use **Workspace settings → Audit log** (Premium plans) to
   enumerate any actions the password took during the
   compromise window. Free plans have no audit log.
4. Issue a replacement with the narrowest permission set the
   integration needs. Consider migrating to a dedicated
   integration user instead of a human's account.

## 3. Quota / rate-limit incidents

Bitbucket Cloud enforces a per-IP-per-hour quota (~1000
req/hr unauthenticated, ~5000 req/hr authenticated). Throttle
signal:

- `429 Too Many Requests` with an `X-RateLimit-Reset` (unix)
  header. The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 1 (=
  ~3600/hr, well below the authenticated cap).
- `pagelen=50` is the maximum on most v2 endpoints;
  pagination follows the response's `next` link field
  (full URL). The connector's delta uses `q=` filtering
  rather than `next`-link traversal so each tick is a
  bounded query.

## 4. Outage detection & recovery

Bitbucket publishes status at https://bitbucket.status.atlassian.com
(rolled up under Atlassian's umbrella status page).

Common outage signatures:

- `503 Service Unavailable` during Atlassian platform
  maintenance windows.
- `502 Bad Gateway` from the CDN edge during certificate
  rotation.
- `429` immediately after Bitbucket clusters re-balance —
  the per-IP counter is reset asymmetrically. Re-trying with
  backoff is sufficient.

Recovery:

1. Confirm via https://bitbucket.status.atlassian.com.
2. Delta resumes from the persisted `updated_on` ISO-8601
   cursor. PRs updated mid-outage are surfaced on the next
   tick. The `q=` filter is server-side and exactly-once if
   the cursor monotonically advances.
3. Bitbucket retains closed/merged PRs indefinitely — the
   connector emits them as `ChangeUpserted` with their
   merge/decline timestamps.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Bad credentials` | App password revoked | §2 |
| `403` | `Repository access denied` | User lost workspace access | Re-add user to workspace |
| `404` | `Repository not found` | Workspace or repo renamed | Update `workspace`/`repo` config |
| `429` | `Rate limit exceeded` | Per-IP/per-user throttle | §3, honor `X-RateLimit-Reset` |
| `500` | `We had a problem` | Bitbucket side, transient | Retry with backoff |
| `503` | `Service Unavailable` | Atlassian-platform maintenance | §4 |
