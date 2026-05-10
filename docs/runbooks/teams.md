# Microsoft Teams runbook (`teams`)

> Source: [`internal/connector/teams/teams.go`](../../internal/connector/teams/teams.go)
> · API base: `https://graph.microsoft.com/v1.0`
> · Webhook: Microsoft Graph change notifications
>   (`WebhookPath() = "/teams"`)
> · Delta cursor: `/teams/{id}/channels/{id}/messages/delta`

## 1. Auth model

Microsoft Graph OAuth 2 access token, identical shape to
[OneDrive](onedrive.md) and [SharePoint](sharepoint.md). The
credential blob:

```json
{
  "access_token": "eyJ0eXAiOi..."
}
```

Required Graph permissions:

- Application: `ChannelMessage.Read.All`,
  `Chat.Read.All`,
  `Team.ReadBasic.All`. Application-flow is the only way to read
  *all* channels in a team without a delegated user signed in.
- Delegated (per-user fallback): `ChannelMessage.Read`,
  `Chat.Read`, `Team.ReadBasic.All`.

Graph change notifications (Teams webhooks) require:

- A reachable HTTPS endpoint configured as the subscription's
  `notificationUrl`.
- A renewal job — Graph subscriptions expire every 60 minutes for
  `chats` and 3 days for `channels`. The platform's subscription
  renewer (admin job) extends them before expiry.
- A `clientState` shared secret; `HandleWebhook` verifies it on
  every payload.

## 2. Credential rotation

Routine rotation: identical to
[SharePoint §2](sharepoint.md#2-credential-rotation) — same Azure
AD app, same client secret / certificate flow.

Teams-specific:

- After credential rotation, re-create the Graph change
  notification subscription so it is signed with the fresh
  token. Old subscriptions stop delivering once the underlying
  app credential rotates.
- The renewal job in
  [`internal/admin/`](../../internal/admin/) detects expiry from
  the `expirationDateTime` field and PATCHes the subscription
  before the gap.

Emergency rotation:

1. Azure AD → **App registrations** → revoke sessions per
   SharePoint emergency flow.
2. Pause the source.
3. Re-issue credentials, re-create the Graph subscription.
4. Audit `source.connected` between leak time and rotation.

## 3. Quota / rate-limit incidents

Graph throttles per-app per-tenant. Teams endpoints are heavily
metered:

- `429 Too Many Requests` with `Retry-After`.
- `503 Service Unavailable` for backend overload.

Hot endpoints:

- `/teams/{id}/channels` (list channels)
- `/teams/{id}/channels/{id}/messages/delta` (delta messages)
- `/me/chats` (1:1 / group chats)

The connector pages with `@odata.nextLink`; throttling pauses
pagination cleanly.

Knobs:

- Per-source RPS bucket
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go)).
- Reduce `pipeline.StageConfig.FetchWorkers` if Graph is
  throttling globally — quota is per-app, not per-source.

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- Graph subscription stuck on `expired` despite renewal — usually
  an app-permission regression.
- `pipeline.fetch` span error rate spike.

Recovery:

1. Confirm via <https://status.office.com>.
2. If the delta cursor expired (`410 Gone`), reset cursor in the
   admin portal to force a full re-walk.
3. If subscriptions are not delivering, check the `clientState`
   secret hasn't drifted between platform-side and Graph.

## 5. Common error codes

| Upstream | Code | Meaning | Action |
|----------|------|---------|--------|
| `401 Unauthorized` | `InvalidAuthenticationToken` | Token expired | §2 rotation |
| `403 Forbidden` | `Authorization_RequestDenied` | App permission removed | Re-grant in Azure AD; admin re-consent |
| `404 itemNotFound` | n/a | Channel / chat deleted | Emit `ChangeDeleted` |
| `410 Gone` | `resyncRequired` | Delta cursor expired | Reset cursor |
| `429 Too Many Requests` | `tooManyRequests` | Throttle | §3 honor `Retry-After` |
| `503 ServiceUnavailable` | n/a | Graph backend hiccup | Coordinator retry |
| `403` on webhook verification | n/a | `clientState` mismatch | Rotate `clientState`; recreate subscription |
