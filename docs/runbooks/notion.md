# Notion runbook (`notion`)

> Source: [`internal/connector/notion/notion.go`](../../internal/connector/notion/notion.go)
> · API base: `https://api.notion.com/v1`
> · Webhook: not used (poll only)
> · Delta cursor: `last_edited_time` filter on `/search`

## 1. Auth model

Internal Notion integration token (`secret_...`). The credential
blob:

```json
{
  "access_token": "secret_..."
}
```

Notion integration tokens never expire on their own; rotation is
admin-driven. The connector sends:

- `Authorization: Bearer secret_...`
- `Notion-Version: 2022-06-28` (or whatever the connector pins —
  inspect `notion.go` for the exact header).

The integration must be **shared** to every page / database it
should ingest (Notion → Share → Connect to integration). Without
the share, the API returns `restricted_resource` 403 even for
the workspace owner.

## 2. Credential rotation

Routine rotation:

1. <https://www.notion.so/my-integrations> → your integration →
   **Capabilities + Secret** → press "Show" then "Refresh secret".
2. PATCH the source credential via the admin portal (re-encrypted
   in transit by [`internal/credential/`](../../internal/credential/)).
3. Verify: `GET /users/me` → `{"object":"user", "type":"bot"}`.

Emergency rotation:

1. Refresh the secret per the routine flow — there is no
   per-token revoke endpoint in Notion's public API; the refresh
   action invalidates the previous token immediately.
2. Pause the source until the new token is in place.
3. Audit the `source.connected` events for the source between
   leak time and rotation.

## 3. Quota / rate-limit incidents

Notion enforces ~3 requests / second per integration. Symptoms:

- `429 Too Many Requests` with body
  `{"object":"error", "status":429, "code":"rate_limited"}`.
- The connector forwards the error; the platform-side retry
  budget in
  [`internal/pipeline/coordinator.go`](../../internal/pipeline/coordinator.go)
  retries with exponential backoff.

Knobs:

- Per-source RPS via the Redis token bucket in
  [`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go).
  Set the bucket to ≤ 2 RPS to stay safely below Notion's
  threshold.
- Notion's pagination is keyset (cursor), so a throttled cycle
  resumes cleanly without losing pages.

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- 5xx errors in the Stage 1 fetch span (`pipeline.fetch`).

Recovery:

1. Confirm via <https://status.notion.so>.
2. If the integration was un-shared by accident, the connector
   surfaces `restricted_resource` 403 — re-share the page /
   database from Notion's UI and the next poll picks it up.

## 5. Common error codes

| Upstream | Code | Meaning | Action |
|----------|------|---------|--------|
| `401` | `unauthorized` | Token rotated upstream | §2 rotation |
| `403` | `restricted_resource` | Integration not shared with the resource | Re-share in Notion UI |
| `404` | `object_not_found` | Page / database deleted upstream | Emit `ChangeDeleted` next pass |
| `429` | `rate_limited` | RPS exceeded | §3 backoff |
| `500` | `internal_server_error` | Backend hiccup | Coordinator retry |
| `503` | `service_unavailable` | Backend backlog | Coordinator retry |
