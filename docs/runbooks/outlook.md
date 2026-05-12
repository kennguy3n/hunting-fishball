# Microsoft 365 Outlook runbook (`outlook`)

> Source: [`internal/connector/outlook/outlook.go`](../../internal/connector/outlook/outlook.go)
> · API base: `https://graph.microsoft.com/v1.0`
> · Delta cursor: `@odata.deltaLink` on
>   `/me/mailFolders/<folder>/messages/delta` or
>   `/users/<id>/mailFolders/<folder>/messages/delta`

## 1. Auth model

OAuth 2.0 Bearer token minted from an Entra app registration
with the **`Mail.Read`** delegated scope (or the application
scope `Mail.Read` for unattended ingestion). The connector
accepts a pre-minted access token.

```json
{ "access_token": "eyJ0eXAi...", "user": "alice@contoso.com" }
```

`user` is optional — when omitted, requests target `/me`
(delegated flow). Header:
`Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation:

1. **Entra admin centre → App registrations → <app> →
   Certificates & secrets → New client secret** (delegated)
   *or* **API permissions → Mail.Read (Application) → Grant
   admin consent** (app-only).
2. Update the secret-manager record; the access-token mint
   job picks it up.
3. Verify: `GET /v1.0/me` (delegated) or `GET /v1.0/users/<id>`
   (app-only) returns `200 OK`.
4. Revoke the previous secret.

Emergency rotation:

1. **App registrations → <app> → Certificates & secrets** →
   delete the leaked secret.
2. Pause every Outlook source bound to the affected app.
3. Audit **Entra → Sign-in logs** + **Security & compliance →
   Audit log** for `MailItemsAccessed` events using the
   service principal during the leak window.
4. If a privileged mailbox (M365 Global Admin / compliance
   admin) was touched, follow Microsoft's mailbox compromise
   IR playbook before resuming the source.

## 3. Quota / rate-limit incidents

Graph applies a 10 000 requests / 10 minutes per-app throttle
plus mailbox-specific limits (≤ 4 concurrent delta sessions per
mailbox).

Throttle signal:

- `429 Too Many Requests` with `Retry-After` and
  `x-ms-throttle-information`.
- `503 Service Unavailable` with `MailboxConcurrency` body for
  mailbox-level concurrency caps.
- Both are wrapped to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 8.
- Limit page size to `$top=50` (Outlook's safe default) —
  larger pages amplify mailbox concurrency pressure.

## 4. Outage detection & recovery

Microsoft publishes Outlook / Exchange Online status at
https://status.cloud.microsoft (M365 admin centre → Service
health → Exchange Online).

Common outage signatures:

- `503` from `/me/messages` during Exchange Online regional
  failovers.
- `404` `MailboxNotEnabledForRESTAPI` if the mailbox was moved
  to a tenant whose REST API endpoint isn't yet provisioned.

Recovery:

1. Confirm via M365 admin centre.
2. Delta poll resumes from the persisted `@odata.deltaLink`
   (valid ≥ 30 days). Mid-outage delta links survive a brief
   regional failover.
3. Deletes propagate as `ChangeDeleted` once the delta surfaces
   the `@removed` tombstone.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":{"code":"InvalidAuthenticationToken"}}` | Bearer expired | §2 routine rotation |
| `403` | `{"error":{"code":"ErrorAccessDenied"}}` | Scope `Mail.Read` not granted | Re-consent the app |
| `404` | `MailboxNotEnabledForRESTAPI` | Tenant API endpoint not provisioned | Wait / re-issue mailbox |
| `410` | `SyncStateInvalid` | Delta token expired | Reset cursor (force bootstrap) |
| `429` | `ApplicationThrottled` | Graph per-app cap | §3 |
| `503` | `MailboxConcurrency` | Mailbox-level concurrency | §3 / §4 |
