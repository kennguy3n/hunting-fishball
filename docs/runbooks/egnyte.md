# Egnyte runbook (`egnyte`)

> Source: [`internal/connector/egnyte/egnyte.go`](../../internal/connector/egnyte/egnyte.go)
> · API base: `https://<domain>.egnyte.com/pubapi`
> · Delta cursor: opaque numeric event id from
>   `/pubapi/v2/events`.

## 1. Auth model

Egnyte issues OAuth 2.0 access tokens. The platform's
token-refresh worker keeps tokens fresh.

```json
{
  "domain":       "acme",
  "access_token": "...",
  "root_path":    "/Shared"
}
```

Header: `Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation:

1. **Egnyte → Admin → Apps & integrations → Manage apps** →
   regenerate the OAuth client secret on the platform's
   registered app.
2. Update the secret-manager record.
3. Verify: `GET /pubapi/v1/userinfo` returns the
   service-account profile.
4. Tokens issued before the secret rotation auto-expire at
   their TTL — no explicit revoke is required.

Emergency rotation:

1. **Admin → Apps & integrations → Manage apps** → revoke
   the OAuth app.
2. Pause every Egnyte source.
3. Re-register the app and reauthorize the integration user.

## 3. Quota / rate-limit incidents

Egnyte publishes per-app daily quotas and per-second
throttling. Throttle signal:

- `403 Forbidden` with `developer_over_qps` in the body for
  per-second throttle.
- `403 Forbidden` with `developer_over_rate` for daily quota.
- The connector wraps both as
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go)
  (the adaptive limiter inspects the body even though the
  status is `403`).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 2 —
  Egnyte's documented limit is 2 rps per app for free tiers.
- For higher-volume accounts, request an app-key uplift via
  Egnyte support; document the new ceiling here.

## 4. Outage detection & recovery

Egnyte publishes status at https://status.egnyte.com.

Common outage signatures:

- `503 Service Unavailable` during regional storage failover.
- Authentication redirects to a maintenance page.

Recovery:

1. Confirm via the status page.
2. Resume from the persisted event-cursor — Egnyte's event
   stream is durable and idempotent.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"errorMessage":"Token expired"}` | OAuth token expired | Refresh trigger |
| `403` | `{"errorMessage":"developer_over_qps"}` | Per-second throttle | §3 |
| `403` | `{"errorMessage":"developer_over_rate"}` | Daily quota | §3 |
| `404` | `{"errorMessage":"FOLDER_NOT_FOUND"}` | Path deleted | Iterator skips |
| `429` | n/a | Reserved (rare) | §3 |
| `503` | n/a | Regional outage | §4 |
