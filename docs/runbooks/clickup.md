# ClickUp runbook (`clickup`)

> Source: [`internal/connector/clickup/clickup.go`](../../internal/connector/clickup/clickup.go)
> · API base: `https://api.clickup.com/api/v2`
> · Webhook: ClickUp webhooks (planned for a later round)
> · Delta cursor: `date_updated_gt=<ms-epoch>` against `/list/<id>/task`

## 1. Auth model

Personal API token (NO `Bearer` prefix — raw value in
`Authorization`).

```json
{ "api_key": "pk_...", "team_id": "9012345" }
```

Header: `Authorization: pk_...`.

Required scopes: the personal token inherits the user's
permissions; the user must have read access to every Space /
Folder / List the source surfaces.

## 2. Credential rotation

Routine rotation:

1. ClickUp → **Settings** → **Apps** → **API Token** → **Generate**
   (replaces the previous token immediately).
2. PATCH the source credential via the admin portal.
3. Verify: `GET /api/v2/user` returns the user payload.

Emergency rotation:

1. Same path as routine — clicking **Generate** invalidates the
   previous value, so there is no separate "revoke" step.
2. Pause the source while regenerating.
3. Audit ClickUp's **Activity Log** for actions taken with the
   leaked token during the exposure window.

## 3. Quota / rate-limit incidents

ClickUp enforces 100 req/min per token (200/min on Enterprise).

Throttle signal:

- `429 Too Many Requests` with `X-RateLimit-Reset` (unix seconds)
  in the response header.
- Some endpoints return `429` with `{"err":"Rate limit reached"}`.

Both surfaces wrap to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 1.5 (matches
  100/min ≈ 1.66 req/s).
- Reduce list namespace count if a single Workspace surfaces
  > 100 lists.

## 4. Outage detection & recovery

ClickUp publishes status at https://status.clickup.com.

Common outage signatures:

- `502 Bad Gateway` from `/api/v2/team/<id>/space` during ClickUp's
  weekly deploys.
- `504 Gateway Timeout` from `/list/<id>/task` when a Workspace has
  > 100k tasks and the server-side query times out — narrow the
  list by archiving stale tasks.

Recovery:

1. Confirm via the status page.
2. Delta poll resumes from the last persisted `date_updated`.
3. For prolonged outages, monitor task-search latency — ClickUp
   sometimes degrades search before the REST API itself.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"err":"Token invalid"}` | Token revoked | §2 routine rotation |
| `403` | `{"err":"Team not authorized"}` | User lost access | Re-invite the user to the Workspace |
| `404` | `{"err":"List not found"}` | List archived / deleted | Remove the namespace from the source |
| `429` | `{"err":"Rate limit reached"}` | Per-minute limit exceeded | §3 |
| `500` | n/a | Upstream incident | §4 |
| `504` | n/a | Search timeout | Narrow the list scope |
