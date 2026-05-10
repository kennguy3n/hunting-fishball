# Slack runbook (`slack`)

> Source: [`internal/connector/slack/slack.go`](../../internal/connector/slack/slack.go)
> · API base: `https://slack.com/api`
> · Webhook: Slack Events API (`WebhookPath() = "/slack"`)
> · Delta cursor: `oldest`/`latest` timestamps over `conversations.history`

## 1. Auth model

Slack bot OAuth token (`xoxb-...`) issued to a Slack app installed
in the workspace. The credential blob:

```json
{
  "access_token": "xoxb-...",
  "signing_secret": "<for webhook signature verification>"
}
```

Required Slack scopes (set on the app's OAuth settings page):

- `channels:read`, `groups:read`, `im:read`, `mpim:read` — list
  channels.
- `channels:history`, `groups:history` — read messages /
  threads.
- `users:read` — populate workspace user identities.

The connector adds `Authorization: Bearer xoxb-...` and
`Content-Type: application/json` to every call. Webhook payloads
are verified by signing-secret HMAC before being decoded by
`HandleWebhook`.

## 2. Credential rotation

Routine rotation:

1. In Slack admin → **Apps** → your hunting-fishball app →
   **OAuth & Permissions** → press **Reinstall to Workspace** to
   mint a fresh `xoxb-...` (re-confirms scopes).
2. PATCH the source credential via the admin portal; the platform
   re-encrypts and persists.
3. Verify: `POST https://slack.com/api/auth.test` should return
   `{"ok": true, "user_id": "U..."}`.
4. The Events-API subscription URL stays the same; no Slack-side
   re-subscribe is required for token rotation.

Emergency rotation (token leak):

1. Slack admin → **Apps** → **Manage** → revoke the bot
   installation immediately. This invalidates `xoxb-...`.
2. Pause the source in the admin portal.
3. Reinstall the app per the routine flow.
4. Rotate the **signing secret** too (App Credentials →
   "Verification Token / Signing Secret"). Old signed webhook
   payloads stop verifying immediately, so updating both halves
   atomically avoids a window where an attacker could replay a
   leaked secret.

## 3. Quota / rate-limit incidents

Slack tiers: methods are bucketed Tier 1 (1 / minute) through
Tier 4 (100+ / minute). The connector hits Tier 3 / 4 endpoints
(`conversations.list`, `conversations.history`,
`conversations.replies`).

Slack returns:

- `429 Too Many Requests` with a `Retry-After` header.
- `200` with `{"ok": false, "error": "ratelimited"}` for the same
  condition on some endpoints.

Knobs:

- The platform-side rate limiter
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go))
  enforces a per-source RPS via Redis token bucket. Tune
  `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` below 50 to stay
  inside Tier 3 budgets.
- For very large workspaces, use the connector's namespace filter
  (admin portal → "Channels to ingest") to scope ingestion to
  channels the bot is invited to. The bot is NOT auto-added to
  channels — the workspace admin must `/invite @bot` per channel.

## 4. Outage detection & recovery

Slack outages are status.slack.com-tracked; check there first.
Common outage signatures the connector observes:

- `503 Service Unavailable` from `slack.com/api/*`.
- Webhook deliveries paused (Slack retries with exponential
  backoff for up to 24h).

Recovery:

1. Confirm Slack-side outage via status page; if confirmed, the
   coordinator's retry budget will absorb most of the gap and
   resume on its own.
2. If the outage exceeds Slack's webhook retry window, force a
   delta poll: in the admin portal → "Re-sync now" — this calls
   `DeltaSync` with the persisted `oldest` cursor and replays
   any missed messages.
3. Cross-check the audit log for `chunk.indexed` events resuming;
   absence after recovery means the consumer is wedged — bounce
   `context-engine-ingest`.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"ok":false,"error":"invalid_auth"}` | Token revoked | §2 routine rotation |
| `200` | `{"ok":false,"error":"missing_scope","needed":"channels:history"}` | Scope removed during reinstall | Re-grant on the OAuth & Permissions page |
| `200` | `{"ok":false,"error":"ratelimited"}` | Slack tier exceeded | §3; backoff is honored by `Retry-After` |
| `429` | n/a | Same as above (some endpoints emit HTTP 429) | §3 |
| `200` | `{"ok":false,"error":"channel_not_found"}` | Bot lost membership | Re-invite via Slack `/invite @bot` |
| `403` | `{"error":"signature_mismatch"}` | Webhook HMAC mismatch | Rotate signing secret per §2 emergency flow |
