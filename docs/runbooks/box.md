# Box runbook (`box`)

> Source: [`internal/connector/box/box.go`](../../internal/connector/box/box.go)
> · API base: `https://api.box.com/2.0`
> · Webhook: not used (poll only)
> · Delta cursor: `/events?stream_position=<pos>`

## 1. Auth model

OAuth 2 access token issued to a Box app (custom app or
limited-access app). The credential blob:

```json
{
  "access_token": "<box-access-token>"
}
```

Box access tokens are short-lived (60 minutes). The platform
credential service refreshes via the `client_credentials` flow
(JWT app) or `refresh_token` flow (OAuth app).

Required app scopes:

- `Read all files and folders stored in Box`
- `Manage enterprise properties` only if the source spans an
  enterprise; not needed for individual user accounts.

`Validate` rejects empty `access_token`. The connector adds
`Authorization: Bearer <token>` to every call (`do` helper) and
follows redirects on `/files/{id}/content` to the short-lived
download URL.

## 2. Credential rotation

Routine rotation:

1. Box Developer Console → **My Apps** → app → **Configuration**
   → mint a fresh client secret OR rotate the JWT key pair.
2. Re-run the platform's credential exchange to mint a fresh
   `access_token`.
3. PATCH the source credential.
4. Verify: `GET /users/me` → `{"id": "<user-id>", "type":
   "user"}`.

Emergency rotation (suspected leak):

1. Box admin console → **Apps** → revoke the app's authorization
   for the enterprise. This invalidates *every* access token the
   app has minted.
2. Pause the source.
3. Re-grant authorization + re-run the credential exchange.
4. Force a forget-on-removal-style audit if the leak required a
   re-ingest: pause, run the forget-worker for the source, then
   resume.

## 3. Quota / rate-limit incidents

Box quotas:

- 1000 requests / minute per access token.
- 100 concurrent uploads / downloads per app.

The connector hits `/folders/{id}/items` for namespace listing,
`/files/{id}` for metadata, `/files/{id}/content` for bytes, and
`/events` for delta. Box returns:

- `429 Too Many Requests` with `Retry-After: <seconds>`.
- `503 Service Unavailable` for backend overload.

Knobs:

- Per-source RPS via the platform Redis token bucket
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go)).
- Reduce `pipeline.StageConfig.FetchWorkers` if Box is throttling
  globally — the bucket only paces individual sources, not the
  app-wide budget.

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- `box: events status=...` errors in the Stage 1 fetch span.

Recovery:

1. Confirm via <https://status.box.com>.
2. If the `next_stream_position` is rejected (Box returns 400 on
   stream positions older than ~2 weeks), reset cursor in the
   admin portal: the connector re-walks
   `/folders/0/items?recursive=true` and emits `ChangeAdded` for
   every file as a recovery sync.

## 5. Common error codes

| Upstream | Meaning | Action |
|----------|---------|--------|
| `401 Unauthorized` | Token expired | §2 rotation |
| `403 Forbidden`, `code=access_denied_insufficient_permissions` | App scope missing | Re-grant scopes in Box Developer Console |
| `404 not_found` on FetchDocument | File trashed upstream | Connector emits `ChangeDeleted` |
| `429 Too Many Requests` | Token-bucket exhaustion | §3; honor `Retry-After` |
| `503 Service Unavailable` | Backend overload | Coordinator retry |
| `400 invalid_grant` (refresh) | Refresh token expired | §2 emergency rotation |
