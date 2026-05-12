# Mattermost runbook (`mattermost`)

> Source: [`internal/connector/mattermost/mattermost.go`](../../internal/connector/mattermost/mattermost.go)
> · API base: `https://<host>/api/v4`
> · Webhook: outgoing webhooks + slash commands (not yet wired into the connector)
> · Delta cursor: `since=<ms-epoch>` against `/channels/<id>/posts`

## 1. Auth model

Personal access token issued by the workspace admin.

```json
{ "access_token": "kt-...", "base_url": "https://chat.acme.com" }
```

Header: `Authorization: Bearer kt-...`.

Required role: at least `system_user` with **personal access token**
enabled in **System Console → Integrations → Integration
Management**.

## 2. Credential rotation

Routine rotation:

1. Mattermost → **Account Settings** → **Security** → **Personal
   Access Tokens** → **Generate New Token**.
2. PATCH the source credential via the admin portal.
3. Verify: `GET /api/v4/users/me` returns the bot user.
4. Revoke the previous token in the same panel.

Emergency rotation:

1. Workspace admin → **Account Settings** → **Security** →
   revoke the leaked token.
2. Pause the source.
3. Generate a fresh token and patch the credential.
4. Audit Mattermost's **Compliance Export** for any actions
   taken by the bot during the leak window.

## 3. Quota / rate-limit incidents

Mattermost's default REST rate limit is 10 req/s per IP / 30 burst
(set in `config.json` `RateLimitSettings`). Self-hosted clusters
may raise this; ask the workspace admin.

Throttle signal:

- `429 Too Many Requests` with `Retry-After` header.
- Self-hosted clusters may return `503 Service Unavailable`
  during plugin reloads — these are NOT throttling; do not bump
  the rate limiter.

Wraps to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go)
on 429; the iterator pauses according to adaptive_rate.go.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 5 (half the
  default).
- Filter the channel namespace list to the most-trafficked
  channels to reduce burst.

## 4. Outage detection & recovery

Mattermost Cloud publishes status at https://status.mattermost.com;
self-hosted deployments expose `/api/v4/system/ping`.

Common outage signatures:

- `/api/v4/users/me` returns 401 even with a known-good token →
  the workspace's licence expired (token-auth disabled).
- `/api/v4/channels/*/posts` returns `503` during a database
  failover — the connector retries via the iterator.

Recovery:

1. Confirm via the status page or the `/system/ping` health check.
2. Delta poll resumes from the last persisted `update_at` cursor.
3. For prolonged outages, the `since` window can grow — adjust
   the source's `delta_max_lookback_hours` knob temporarily.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"id":"api.context.session_expired.app_error"}` | Token revoked | §2 routine rotation |
| `403` | `{"id":"api.channel.get_channels.permissions.app_error"}` | Bot not in the channel | Invite the bot or remove the channel from the source |
| `404` | `{"id":"app.user.missing_account.app_error"}` | Workspace deleted | Pause source; confirm with admin |
| `429` | `{"id":"api.rate_limit_exceeded"}` | Burst limit exceeded | §3; honour `Retry-After` |
| `500` | `{"id":"app.post.get_posts.app_error"}` | Database transient | Retries handled by the iterator |
| `503` | n/a | Cluster restart / plugin reload | §4 |
