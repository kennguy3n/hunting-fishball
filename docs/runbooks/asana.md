# Asana runbook (`asana`)

> Source: [`internal/connector/asana/asana.go`](../../internal/connector/asana/asana.go)
> · API base: `https://app.asana.com/api/1.0`
> · Webhook: not implemented (Asana supports webhooks; planned for a later round)
> · Delta cursor: `modified_since=<RFC3339>` on `/projects/<gid>/tasks`

## 1. Auth model

Asana personal access token (PAT) or service-account token.

```json
{ "access_token": "2/12345/..." }
```

Header: `Authorization: Bearer 2/12345/...`.

## 2. Credential rotation

Routine rotation:

1. Asana → **My Profile** → **Apps** → **Manage Developer Apps**
   → **Personal Access Tokens** → **Create new token**.
2. PATCH the source credential via the admin portal.
3. Verify: `GET /users/me` returns the bot's user record.
4. Revoke the old token from the same page.

Emergency rotation:

1. Revoke the leaked token immediately.
2. Pause the source.
3. Create a replacement PAT, scoped narrowly to read-only if the
   compromised token had write scopes.

## 3. Quota / rate-limit incidents

Asana enforces a per-token bucket: 150 requests / minute for
free, 1500 / minute for paid workspaces. The connector hits
`/users/me`, `/workspaces`, `/projects`, `/projects/<gid>/tasks`,
`/tasks/<gid>`.

Throttle signal:

- `429 Too Many Requests` with `Retry-After`.

Wraps to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` defaults to 2.5
  (matches 150/min); raise for paid plans.
- Restrict to specific projects in the namespace filter to avoid
  pulling every workspace.

## 4. Outage detection & recovery

Asana publishes status at https://trust.asana.com/.

Common outage signatures:

- `5xx` from `app.asana.com/api/1.0/*`.
- HTML responses (rate-limit page or CloudFlare challenge) —
  connector treats non-2xx as failure.

Recovery:

1. Confirm via trust.asana.com.
2. Once recovered, the delta poll cycle picks up where it left
   off via `modified_since`.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"errors":[{"message":"Not Authorized"}]}` | Token revoked | §2 routine rotation |
| `403` | `{"errors":[{"message":"Forbidden"}]}` | Workspace access removed | Re-add bot user to workspace; recreate PAT |
| `404` | `{"errors":[{"message":"Not Found"}]}` | Project deleted | Remove from namespace filter |
| `429` | n/a | Per-token quota exceeded | §3; honour `Retry-After` |
| `500` | `{"errors":[{"message":"..."}]}` | Upstream incident | §4 |
