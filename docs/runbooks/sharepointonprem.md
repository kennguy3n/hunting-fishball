# SharePoint on-prem runbook (`sharepoint_onprem`)

> Source: [`internal/connector/sharepoint_onprem/sharepoint_onprem.go`](../../internal/connector/sharepoint_onprem/sharepoint_onprem.go)
> · API base: `https://<host>/<site>/_api`
> · Delta cursor: OData `Modified gt datetime'<ISO8601>'`
>   against `/_api/web/lists(guid'<id>')/items`.

## 1. Auth model

SharePoint Server / on-prem accepts either a service-account
username + password (Basic over TLS) or an application
password issued via the SharePoint app catalog.

```json
{
  "base_url":      "https://sp.example.com",
  "site_path":     "/sites/team",
  "username":      "DOMAIN\\svc-context",
  "password":      "...",
  "app_password":  ""
}
```

Headers:

- `Authorization: Basic base64(user:pass)` when `app_password`
  is empty.
- `Authorization: Bearer <app_password>` for app-password mode.
- `Accept: application/json;odata=verbose` on every call.

## 2. Credential rotation

Routine rotation (service-account):

1. **SharePoint Central Admin → Security → Manage service
   accounts** → reset the password for `svc-context`.
2. Update the secret-manager record.
3. Verify: `GET /_api/web` returns the site title.
4. Invalidate any session held in the upstream auth provider
   (Kerberos ticket cache, ADFS refresh token).

Routine rotation (app-password):

1. **Site → Settings → Site app permissions → Generate new
   app password**.
2. Update the secret-manager record.
3. Verify, then revoke the prior app-password.

Emergency rotation:

1. Disable the upstream account in AD.
2. Pause every on-prem SharePoint source.
3. Reissue per the routine flow once the AD account is
   re-enabled.

## 3. Quota / rate-limit incidents

SharePoint on-prem throttling surfaces as `429 Too Many
Requests` or `503 Service Unavailable` with the
`Retry-After` header populated.

- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).
- Honor `Retry-After` (seconds) before resuming.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 4.
- The REST API does NOT publish a public RPS — SharePoint
  documents the throttling policy without numbers; treat
  observed 429 rate as the truth.

## 4. Outage detection & recovery

On-prem SharePoint outages are operator-visible:

- `503` with `Retry-After` during content database
  maintenance windows.
- TLS handshake failures during certificate rotation on the
  reverse proxy.

Recovery:

1. Confirm with the on-prem SharePoint admin.
2. The delta cursor is a server-side `Modified` timestamp;
   resume picks up where it left off.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":{"code":"AuthenticationFailed"}}` | Password rotated / Kerberos expired | §2 routine rotation |
| `403` | `{"error":{"code":"AccessDenied"}}` | Service account dropped from list ACL | Re-grant list-level Reader |
| `404` | n/a | List or item deleted | Iterator skips |
| `429` | n/a | Throttled | §3 |
| `503` | n/a | Content DB failover | §4 |
