# OneDrive runbook (`onedrive`)

> Source: [`internal/connector/onedrive/onedrive.go`](../../internal/connector/onedrive/onedrive.go)
> · API base: `https://graph.microsoft.com/v1.0`
> · Webhook: not used (poll only)
> · Delta cursor: Microsoft Graph delta token (`/me/drive/root/delta`)

## 1. Auth model

Microsoft Graph OAuth 2 access token bound to a single user. The
credential blob:

```json
{
  "access_token": "eyJ0eXAiOi..."
}
```

Required delegated Graph permissions:

- `Files.Read` (read user's own files)
- `Files.Read.All` (read shared files; required if the user has
  shared OneDrive folders the platform should ingest)
- `User.Read`

`Validate` rejects an empty `access_token`. The connector hits
`/me/drive/root/children` for namespace listing and
`/me/drive/items/{id}` for individual files.

## 2. Credential rotation

Routine rotation: identical to [SharePoint §2](sharepoint.md#2-credential-rotation)
because both connectors share Graph and Azure AD.

OneDrive-specific:

1. The OAuth client is typically a **public client** (mobile /
   desktop) when ingesting personal OneDrive, so secret rotation
   is replaced by the platform's refresh-token round-trip.
2. After a fresh `access_token` is minted, verify with
   `GET /me/drive` — should return the drive's quota.

Emergency rotation: same as SharePoint. Additionally consider
revoking refresh tokens (`POST /me/revokeSignInSessions`) to
invalidate every session for the bound user.

## 3. Quota / rate-limit incidents

OneDrive shares Graph's throttling. The hot endpoints are
`/me/drive/root/children` and `/me/drive/items/{id}/content`. The
content endpoint redirects to a short-lived
`https://onedrive-files.live.com` URL — the connector follows
redirects and reuses the same `Authorization` header (see
`do` helper in `onedrive.go`).

Knobs:

- Per-source RPS bucket
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go)).
- For a tenant with many user-scoped OneDrive sources, the global
  Graph quota is per-app, not per-user. Reducing total parallelism
  helps; per-source scaling does not.

## 4. Outage detection & recovery

Same surface as SharePoint:

- `internal/admin/health.go` last-success staleness alert.
- Graph status page.

Recovery:

1. Confirm via status page.
2. If only one user is affected, check if the user's OneDrive is
   provisioned (`GET /me/drive` returning 404 means no provisioned
   drive — common for service accounts).
3. If healthy from the control plane but the delta token is
   stale, the connector handles `410 resyncRequired` by resetting
   the cursor and emitting full re-sync change records.

## 5. Common error codes

| Upstream | Meaning | Action |
|----------|---------|--------|
| `401 Unauthorized` | Token expired | §2 rotation |
| `403 Forbidden`, `code=accessDenied` | User revoked OAuth consent | Pause source; require user re-consent |
| `404 itemNotFound` | File deleted upstream | Connector emits `ChangeDeleted` |
| `410 Gone`, `code=resyncRequired` | Delta token expired | Reset cursor; full re-sync |
| `429 Too Many Requests` | Graph throttle | §3 |
| `507 Insufficient Storage` | Drive quota exceeded (writes) | N/A — connector is read-only |
