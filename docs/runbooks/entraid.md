# Microsoft Entra ID runbook (`entra_id`)

> Source: [`internal/connector/entra_id/entra_id.go`](../../internal/connector/entra_id/entra_id.go)
> · API base: `https://graph.microsoft.com/v1.0`
> · Delta cursor: Microsoft Graph `@odata.deltaLink` /
>   `$deltatoken` on `/users/delta` + `/groups/delta`

## 1. Auth model

Microsoft Entra ID (formerly Azure AD) uses OAuth 2.0 client
credentials. The connector accepts a pre-minted access token
(short-lived) with scopes `Directory.Read.All` and
`Group.Read.All` granted to the application registration.

```json
{ "access_token": "eyJ0eXAiOi..." }
```

Header: `Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation:

1. **Entra admin centre → App registrations → <app> → Certificates
   & secrets → New client secret**.
2. Update the secret-manager record for the app's client secret;
   the access-token mint job picks it up on the next refresh.
3. Verify: `GET /v1.0/organization?$top=1` returns the tenant
   org with a `200 OK`.
4. Revoke the prior secret in the same blade once the new token
   has propagated.

Emergency rotation:

1. **App registrations → <app> → Certificates & secrets** →
   delete the leaked secret.
2. Pause every source bound to the affected client.
3. Audit **Microsoft Entra → Monitoring → Sign-in logs** for
   service-principal sign-ins using the leaked credential.
4. File a Microsoft 365 admin ticket if the leak touched
   privileged AAD roles.

## 3. Quota / rate-limit incidents

Graph applies a 10 000 requests / 10 minutes per-app throttle and
per-tenant `/users` reads cap (varies). Throttle signal:

- `429 Too Many Requests` with `Retry-After` (seconds) and
  `x-ms-throttle-limit-percentage`.
- The connector wraps both `429` and the rare `503 Service
  Unavailable: TenantThrottled` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 8 (matches
  the 10k/10m sustained ceiling at ~16 rps with headroom).
- For very large tenants, page through `/users?$top=999` (the
  Graph maximum) to reduce request count.

## 4. Outage detection & recovery

Microsoft publishes Graph status at
https://status.cloud.microsoft (Microsoft 365 admin centre →
Service health → Microsoft 365 Apps / Microsoft Entra).

Common outage signatures:

- `503 Service Unavailable` from `/users` during Graph regional
  failovers.
- `403 Forbidden` `Authorization_RequestDenied` if Conditional
  Access blocked the service principal during a policy push.

Recovery:

1. Confirm via Microsoft 365 admin centre.
2. Delta poll resumes from the persisted `@odata.deltaLink`.
   Graph delta links are valid for ≥ 30 days so an outage of a
   few hours never resets the cursor.
3. Deprovisioned / disabled users (`accountEnabled=false`,
   `@removed`) are emitted as `ChangeDeleted` so downstream
   indexes converge automatically.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":{"code":"InvalidAuthenticationToken"}}` | Bearer expired/revoked | §2 routine rotation |
| `403` | `{"error":{"code":"Authorization_RequestDenied"}}` | App lacks `Directory.Read.All` | Re-consent via admin |
| `403` | `{"error":{"code":"Authorization_IdentityNotFound"}}` | Service principal deleted | Re-create app registration |
| `404` | `{"error":{"code":"Request_ResourceNotFound"}}` | User/group GC'd | Iterator skips; delta will emit `ChangeDeleted` |
| `429` | `{"error":{"code":"TooManyRequests"}}` | Graph throttle | §3 |
| `503` | `{"error":{"code":"ServiceNotAvailable"}}` | Graph cell failover | §4 |
