# Dropbox runbook (`dropbox`)

> Source: [`internal/connector/dropbox/dropbox.go`](../../internal/connector/dropbox/dropbox.go)
> · API base: `https://api.dropboxapi.com/2`
> · Webhook: not used (poll only)
> · Delta cursor: `/files/list_folder/get_latest_cursor` → `/files/list_folder/continue`

## 1. Auth model

OAuth 2 access token issued to a Dropbox app. The credential blob:

```json
{
  "access_token": "sl.XXXX..."
}
```

Dropbox tokens are short-lived (4 hours) by default. The platform
credential service refreshes the token before expiry using the
stored `refresh_token` and `app_key`/`app_secret`.

Required scope set on the Dropbox app:

- `files.metadata.read`
- `files.content.read`
- `account_info.read` (for `/users/get_current_account`)

The connector POSTs JSON bodies to every endpoint and uses
`Authorization: Bearer <access_token>`.

## 2. Credential rotation

Routine rotation:

1. Dropbox App Console → **Permissions** → confirm scopes
   unchanged → **OAuth 2** → mint a fresh `app_secret` if needed.
2. Re-run the OAuth flow from the admin portal "Re-authorize"
   button to mint a fresh `refresh_token` + `access_token` pair.
3. PATCH the source credential.
4. Verify: `POST /users/get_current_account` (no body) →
   `{"account_id": "dbid:..."}`.

Emergency rotation:

1. Dropbox App Console → **App settings** → "Revoke all OAuth
   tokens for this app". Invalidates `access_token` immediately.
2. Pause the source.
3. Re-mint and re-authorize per the routine flow.

## 3. Quota / rate-limit incidents

Dropbox returns:

- `429 Too Many Requests` with body
  `{"error_summary": "too_many_requests/.", "error":
  {".tag": "too_many_requests", "retry_after": <seconds>}}`.
- `503 Service Unavailable` with optional `Retry-After` header.

Per-app limits: total namespaces × call rate. Avoid tight loops on
`/files/list_folder/continue` — the connector already paginates
with cursors, so the natural pacing matches the API.

Knobs:

- Per-source RPS via the platform Redis token bucket.
- If the bucket throttles hard, the iterator's `cursor` is
  preserved so a resume picks up where it left off.

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- Spike in `dropbox: list_folder status=503` errors in the Stage 1
  fetch span.

Recovery:

1. Confirm via <https://status.dropbox.com>.
2. If the cursor returns `cursor_expired` (after 60+ days
   inactive), reset cursor in the admin portal — the connector
   re-walks `list_folder` with a fresh cursor, treating every
   file as `ChangeAdded`.
3. Cross-check audit log for `chunk.indexed` resumption.

## 5. Common error codes

| Upstream | Meaning | Action |
|----------|---------|--------|
| `401 Unauthorized` | Token expired or revoked | §2 routine rotation (refresh-token round-trip) |
| `409 Conflict`, `error.tag=path/not_found` | Path no longer exists | Connector emits `ChangeDeleted` |
| `409`, `error.tag=path/restricted_content` | DRM-protected content | Skip; surface as `connector.ErrUnsupportedContent` |
| `410 Gone`, `error.tag=cursor_expired` | Cursor older than retention | Reset cursor; full re-sync |
| `429 Too Many Requests` | Rate-limit | Honor `retry_after` field |
| `503 Service Unavailable` | Backend hiccup | Coordinator retry |
