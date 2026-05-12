# Linear runbook (`linear`)

> Source: [`internal/connector/linear/linear.go`](../../internal/connector/linear/linear.go)
> · API base: `https://api.linear.app/graphql`
> · Webhook: Linear webhooks (`WebhookPath() = "/linear"`)
> · Delta cursor: `updatedAt > <RFC3339 timestamp>` GraphQL filter

## 1. Auth model

Linear personal or workspace API key.

```json
{
  "api_key": "lin_api_...",
  "signing_secret": "<for webhook signature verification>"
}
```

Header: `Authorization: lin_api_...` (no Bearer prefix).
GraphQL queries are POSTed to `/graphql` with
`Content-Type: application/json`.

## 2. Credential rotation

Routine rotation:

1. Linear → **Settings** → **Account** → **API** → **Create
   key**; copy the new `lin_api_...`.
2. PATCH the source credential via the admin portal.
3. Verify: a `viewer { id }` GraphQL query returns the bot
   identity.
4. Revoke the old key in **Settings** → **API** → ⋯ →
   **Revoke**.

Emergency rotation:

1. Revoke the leaked key in **Settings** → **API**.
2. Pause the source.
3. Create a replacement; patch credentials; resume.
4. Linear's audit log (Enterprise plan only) records `apiKey.use`
   events — cross-check for unauthorised use.

## 3. Quota / rate-limit incidents

Linear's GraphQL endpoint enforces ~1,500 requests / hour per
API key (free/team plans). Enterprise plans get 5,000+. The
connector consumes ~1 request per page of 50 issues; a workspace
with 10k open issues backfills in ~200 requests.

Throttle signal:

- `429 Too Many Requests` with `Retry-After`.
- GraphQL `errors[0].extensions.code = "RATELIMITED"` on `200`.

Both paths wrap to
[`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floors per-source RPS.
- Filter teams in the namespace config to avoid pulling teams
  the bot doesn't need.

## 4. Outage detection & recovery

Linear publishes status at https://status.linear.app.

Common outage signatures:

- `500/502/503` from `api.linear.app`.
- Persistent GraphQL `internal_server_error`.
- Webhook deliveries paused (Linear retries with exponential
  backoff for up to 72h).

Recovery:

1. Confirm via status page.
2. Once recovered, force a delta poll from the admin portal —
   `DeltaSync` runs `updatedAt > <last cursor>` and replays any
   missed updates.
3. The webhook will replay buffered events automatically. Watch
   `linear_webhook_received_total` and confirm it tracks the
   pre-outage rate.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"errors":[{"message":"Invalid authentication"}]}` | API key revoked | §2 routine rotation |
| `403` | `{"errors":[{"extensions":{"code":"FORBIDDEN"}}]}` | Permission removed from key (e.g. workspace scope) | Re-create key with workspace access |
| `429` | n/a | Per-key hourly bucket exhausted | §3; honour `Retry-After` |
| `200` | `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}` | Same as above, returned as 200 | §3 |
| `200` | `{"errors":[{"extensions":{"code":"INTERNAL_SERVER_ERROR"}}]}` | Upstream incident | §4 |
| webhook `signature_mismatch` | n/a | Linear rotated webhook secret | Update `signing_secret` and re-subscribe |
