# Intercom runbook (`intercom`)

> Source: [`internal/connector/intercom/intercom.go`](../../internal/connector/intercom/intercom.go)
> · API base: `https://api.intercom.io`
> · Delta cursor: `POST /conversations/search` with
>   `updated_at > <unix>` watermark

## 1. Auth model

Intercom REST API v2 authenticates via a bearer access token.
Tokens are minted per-workspace from the developer hub and
carry the integration's installed scopes (read conversations,
read articles, etc.).

```json
{ "access_token": "..." }
```

Header: `Authorization: Bearer <access_token>`.

## 2. Credential rotation

Routine rotation:

1. **Settings → Developer Hub → New app** (or open the existing
   app) → **Authentication** → **Generate new token**.
2. Install the app on the workspace (this is what creates the
   workspace-scoped access token).
3. Update the secret-manager record.
4. Verify with `GET /me` — must return `200 OK` with the
   workspace's `app.id_code`.
5. Uninstall the previous app revision from the workspace to
   revoke the old token (or simply delete the token from the
   developer hub).

Emergency rotation:

1. **Developer Hub → Authentication → Delete token**.
2. Pause the source.
3. Use the **Settings → Workspace data → Audit log** to
   enumerate any actions the token took during the compromise
   window.
4. Issue a replacement token via a fresh app install with the
   narrowest scope set the integration needs.

## 3. Quota / rate-limit incidents

Intercom enforces a per-app per-10s rate limit (default
~166 req/10s = ~1000/min). Throttle signal:

- `429 Too Many Requests` with `X-RateLimit-Reset` (unix
  timestamp) header. The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).
- The `/conversations/search` endpoint is metered separately
  from the simple list endpoints and has a per-page cap of
  150 records.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 10.
- `per_page=50` on list endpoints; `starting_after` cursor in
  the `pages.next` response field for keyset pagination.

## 4. Outage detection & recovery

Intercom publishes status at https://www.intercomstatus.com.

Common outage signatures:

- `503 Service Unavailable` during platform maintenance.
- `502 Bad Gateway` from the CDN edge during certificate
  rotation.
- `200 OK` with a 500ms+ latency spike on `search` — this is
  the indexer trailing the writers; data is correct but the
  delta cursor moves slowly. Wait one tick.

Recovery:

1. Confirm via https://www.intercomstatus.com.
2. Delta resumes from the persisted `updated_at` unix
   timestamp. The search endpoint guarantees exactly-once
   delivery if the cursor monotonically advances.
3. Conversations that received messages mid-outage will
   appear with their post-outage `updated_at` and be picked
   up on the next tick.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Unauthorized` | Token deleted or app uninstalled | §2 |
| `403` | `Forbidden` | Scope missing (e.g. `read_conversations`) | Reinstall app with broader scope |
| `404` | `Resource not found` | Conversation hard-deleted (90 day retention) | Iterator skips |
| `422` | `Unprocessable Entity` | Search query malformed (custom-attribute typo) | Refresh schema, replay |
| `429` | `Too Many Requests` | Per-app throttle | §3, honor `X-RateLimit-Reset` |
| `503` | `Service Unavailable` | Platform maintenance | §4 |
