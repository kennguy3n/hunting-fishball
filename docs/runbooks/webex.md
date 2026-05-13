# Webex runbook (`webex`)

> Source: [`internal/connector/webex/webex.go`](../../internal/connector/webex/webex.go)
> · API base: `https://webexapis.com`
> · Delta cursor: timestamp watermark on `/v1/messages?roomId=<id>`

## 1. Auth model

Webex Messages API authenticates via a bearer token. Tokens
come in two flavours: **bot tokens** (long-lived, scoped to
the bot's user; recommended for service integrations) and
**integration OAuth access tokens** (12h lifetime, refresh
required).

```json
{
  "access_token": "...",
  "room_id":      "..."
}
```

Header: `Authorization: Bearer <access_token>`.

## 2. Credential rotation

Routine rotation (bot token):

1. **https://developer.webex.com/my-apps** → open the bot →
   **Regenerate access token**.
2. Update the secret-manager record.
3. Verify with `GET /v1/people/me` — must return `200 OK`
   with the bot's `type = "bot"`.
4. Regenerate also invalidates the previous token.

Routine rotation (integration OAuth):

1. The token refreshes automatically via the refresh-token
   exchange every 12h. The secret-manager record holds the
   refresh token (90-day lifetime).
2. Rotate the refresh token by re-initiating the OAuth flow
   from the integration's settings page once per quarter.

Emergency rotation:

1. **My Apps → Regenerate** (bot) or **Revoke** (integration).
2. Pause the source.
3. Webex Control Hub → **Analytics → Audit Logs** to enumerate
   any actions the token took during the compromise window.
4. Re-issue with the narrowest space-membership set the
   integration needs.

## 3. Quota / rate-limit incidents

Webex enforces a per-user (bot or human) request rate. The
exact cap is undocumented but typically ~300 req/min for the
Messages API. Throttle signal:

- `429 Too Many Requests` with a `Retry-After` header in
  seconds. The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 4.
- `max=100` on `/v1/messages` is the cap; pagination uses
  `before=<messageId>` for backwards traversal. The
  connector's delta uses a timestamp watermark rather than
  the `before` cursor, so retries are idempotent.

## 4. Outage detection & recovery

Webex publishes status at https://status.webex.com. Regional
outages (e.g. EMEA, APAC) are isolated to a single cluster.

Common outage signatures:

- `503 Service Unavailable` during cluster failover.
- `502 Bad Gateway` from the edge during certificate rotation.
- `409 Conflict` on `POST /v1/messages` when posting to a room
  that's mid-migration between clusters — this is a producer
  signal and doesn't affect the read path.

Recovery:

1. Confirm via https://status.webex.com.
2. Delta resumes from the persisted RFC-3339 `created`
   watermark. Messages posted mid-outage are surfaced on the
   next tick.
3. Webex does not expose deletion timestamps; deleted
   messages drop out of subsequent list responses but the
   connector cannot emit `ChangeDeleted`. Use a periodic
   full re-list to reconcile if deletion fidelity is
   required.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Authentication credentials missing` | Token revoked or expired | §2 |
| `403` | `Insufficient privilege` | Bot/integration not a member of the room | Add bot to the room |
| `404` | `Resource not found` | Room deleted or bot was removed | Verify room state |
| `429` | `Too many requests` | Per-user throttle | §3, honor `Retry-After` |
| `502` | `Bad Gateway` | Edge cert rotation | §4 |
| `503` | `Service Unavailable` | Cluster failover | §4 |
