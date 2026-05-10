# SharePoint Online runbook (`sharepoint`)

> Source: [`internal/connector/sharepoint/sharepoint.go`](../../internal/connector/sharepoint/sharepoint.go)
> · API base: `https://graph.microsoft.com/v1.0`
> · Webhook: not used (poll only)
> · Delta cursor: Microsoft Graph delta token (`/sites/{id}/drive/root/delta`)

## 1. Auth model

Microsoft Graph OAuth 2 access token. The credential blob:

```json
{
  "access_token": "eyJ0eXAiOi...",
  "tenant_id": "<azure-ad-tenant-uuid>"
}
```

The `tenant_id` *inside* the credential is the **Azure AD tenant**
— do **not** confuse it with the hunting-fishball tenant_id passed
through `connector.ConnectorConfig.TenantID`. The latter scopes
storage; the former scopes which Azure AD directory issued the
token.

Required Graph permissions (delegated or application):

- `Sites.Read.All`
- `Files.Read.All`
- `User.Read` (to call `/me`)

The connector adds `Authorization: Bearer <access_token>` to every
Graph call.

## 2. Credential rotation

Routine rotation:

1. Azure AD → **App registrations** → your app → **Certificates
   & secrets** → mint a fresh client secret OR rotate the
   certificate.
2. The platform re-runs the on-behalf-of / client-credentials
   flow through the credential service to mint a new
   `access_token`.
3. PATCH the source credential via the admin portal.
4. Verify with `GET /me` — should return the bound user
   (delegated) or `403` (application token, expected).
5. For application-flow tokens, verify with
   `GET /sites?search=<known-site>` instead.

Emergency rotation (suspected leak):

1. Azure AD → **App registrations** → **Authentication** →
   "Sign out and revoke all sessions". This invalidates *all*
   access tokens for the app.
2. Pause the source.
3. Mint a new client secret + re-run the OAuth flow.
4. Audit the `source.connected` events for the affected source —
   any unexpected `connected` between leak time and rotation
   indicates the leaked token was used.

## 3. Quota / rate-limit incidents

Graph throttles per-app per-tenant. Symptoms:

- `429 Too Many Requests` with `Retry-After: <seconds>`.
- `503 Service Unavailable` with `Retry-After`.

Graph also enforces a **resource-specific** quota. SharePoint
endpoints (`/sites/...`, `/drive/...`) have a separate quota from
e.g. `/users/...`. Check the `RateLimit-Limit` /
`RateLimit-Remaining` response headers in tracing spans to
attribute throttling.

Knobs:

- Per-source RPS via the Redis token bucket (see §1 of
  [`README.md`](README.md)).
- Lower `Concurrency` in the connector by capping
  `pipeline.StageConfig.FetchWorkers` if a tenant has many
  SharePoint sources fan-out simultaneously.

## 4. Outage detection & recovery

Graph status: <https://status.office.com>.

Detection:

- `internal/admin/health.go` — `last_success_at` stale for the
  source.
- A burst of `503` errors in the Stage 1 fetch span.

Recovery:

1. Confirm Graph-side outage via the status page.
2. If a partial-region outage, narrow the source scope to one
   site collection and verify health before widening.
3. If healthy from the control plane but stale: the delta token
   may have rotted (Graph may invalidate after >30 days of
   inactivity). Trigger "Reset cursor" in the admin portal, which
   forces a fresh full sync (`changes.list?$delta=`).

## 5. Common error codes

| Upstream | Meaning | Action |
|----------|---------|--------|
| `401 Unauthorized` | Access token expired or revoked | §2 routine rotation |
| `403 Forbidden`, `code=accessDenied` | Permission removed in Azure AD | Re-grant `Sites.Read.All` and re-consent admin |
| `404 itemNotFound` on FetchDocument | File deleted between list + fetch | Connector emits `ChangeDeleted` next pass |
| `429 Too Many Requests` | Per-app throttle | §3; honor `Retry-After` |
| `410 Gone` (delta endpoint) | Delta token expired | Reset cursor; full re-sync |
| `503 ServiceUnavailable` | Graph backend hiccup | Coordinator retry |
