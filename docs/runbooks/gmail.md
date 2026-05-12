# Gmail runbook (`gmail`)

> Source: [`internal/connector/gmail/gmail.go`](../../internal/connector/gmail/gmail.go)
> · API base: `https://gmail.googleapis.com/gmail/v1`
> · Webhook: Gmail push notifications via Cloud Pub/Sub
>   (planned for a later round)
> · Delta cursor: `history.list?startHistoryId=<id>` (monotonic
>   int64 historyId)

## 1. Auth model

OAuth 2.0 access token (the connector accepts a pre-minted token;
upstream refresh happens in the auth layer).

```json
{ "access_token": "ya29.a0..." }
```

Header: `Authorization: Bearer ya29.a0...`.

Required OAuth scopes:

- `https://www.googleapis.com/auth/gmail.readonly` — read-only
  ingestion.
- `https://www.googleapis.com/auth/gmail.metadata` — optional, if
  the tenant prefers headers-only.

## 2. Credential rotation

Routine rotation (per-user token refresh):

1. The auth service rotates the OAuth refresh token transparently
   — no operator action required for routine refresh cycles.
2. To rotate the *refresh token* itself, revoke at
   https://myaccount.google.com/permissions and re-consent.

Emergency rotation:

1. Workspace admin → **Google Workspace Admin** → **Security**
   → **API controls** → **Manage Third-Party App Access** →
   revoke the OAuth client.
2. Pause the source.
3. Re-issue OAuth credentials and re-consent on behalf of the
   user.
4. Audit **Reporting** → **User Reports** → **Email** for
   actions taken with the leaked token.

## 3. Quota / rate-limit incidents

Gmail enforces a per-user quota of 250 quota-units / second and a
per-project daily quota of 1B units (a typical `messages.list`
call costs 5 units, `messages.get` costs 5).

Throttle signal:

- `429 Too Many Requests` with
  `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`.
- `403 Forbidden` with
  `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}` for
  project-wide quota — the connector wraps both to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 50 (250
  units/s ÷ 5 units/call halved).
- Filter the labelId namespace list (the connector enumerates all
  user labels by default) to ingest only INBOX + STARRED to
  reduce per-poll calls.

## 4. Outage detection & recovery

Google publishes status at https://www.google.com/appsstatus.

Common outage signatures:

- `503 Service Unavailable` from `gmail.googleapis.com/gmail/v1`
  during Google's quarterly maintenance windows.
- `404 Not Found` from `history.list?startHistoryId=<id>` when
  the historyId is older than ~7 days — the connector must
  re-bootstrap (clears the cursor; next call captures the new
  high-water historyId without backfill).

Recovery:

1. Confirm via the status page.
2. If historyId is stale, the connector returns
   `ErrInvalidConfig` on DeltaSync; the orchestrator clears the
   cursor and the next poll bootstraps.
3. For prolonged outages, no manual intervention is required —
   delta resumes from the persisted historyId.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":{"status":"UNAUTHENTICATED"}}` | Token expired | Auth layer refreshes |
| `403` | `{"error":{"errors":[{"reason":"insufficientPermissions"}]}}` | Scope missing | §1 add `gmail.readonly` |
| `403` | `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}` | Project quota | §3 |
| `404` | `{"error":{"errors":[{"reason":"notFound"}]}}` on `history.list` | historyId stale | §4 re-bootstrap |
| `429` | `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}` | Per-user quota | §3 |
| `500` | n/a | Upstream incident | §4 |
