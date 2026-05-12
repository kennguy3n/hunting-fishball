# Salesforce runbook (`salesforce`)

> Source: [`internal/connector/salesforce/salesforce.go`](../../internal/connector/salesforce/salesforce.go)
> · API base: `<instance_url>/services/data/v59.0`
> · Webhook: Salesforce Platform Events / Change Data Capture (planned for a later round)
> · Delta cursor: `SystemModstamp >= <UTC datetime>` SOQL filter

## 1. Auth model

OAuth 2.0 bearer token + instance URL issued by Salesforce's
OAuth flow (typically the `refresh_token` grant operated by the
platform's identity service).

```json
{
  "access_token": "00D...",
  "instance_url": "https://acme.my.salesforce.com",
  "refresh_token": "5Aep...",
  "client_id": "3MVG...",
  "client_secret": "..."
}
```

Header: `Authorization: Bearer 00D...`.

The connector itself doesn't run the refresh-token grant — the
platform's credential plane rotates `access_token` ahead of
expiry and PATCHes the source.

## 2. Credential rotation

Routine rotation:

1. Salesforce → **Setup** → **App Manager** → connected app →
   **Manage Consumer Details** to view client credentials.
2. Trigger the refresh-token grant via the platform's identity
   service; the new `access_token` is written through.
3. Verify: `GET /services/data/v59.0/limits` returns daily API
   call counters.

Emergency rotation:

1. Salesforce → **Setup** → **Connected Apps OAuth Usage** →
   **Revoke** the leaked session.
2. Pause the source.
3. Rotate the connected app's consumer secret in **App Manager**.
4. Re-run the OAuth flow to mint a fresh `refresh_token` +
   `access_token`.

## 3. Quota / rate-limit incidents

Salesforce daily API limits scale with org edition (typically
15k–5M req / 24h). The connector hits `/limits`, `/query`
(SOQL), `/sobjects/<type>/<id>`. Page size on `/query` is 200
records by default, configurable via `LIMIT` clause.

Throttle signal:

- `403 REQUEST_LIMIT_EXCEEDED` (daily bucket).
- `503 SERVER_UNAVAILABLE` (per-org transaction limits).

Wraps to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floors per-source
  RPS to spread the daily bucket evenly.
- Limit SObject types to those actually needed (Account,
  Contact, Opportunity) — backfilling Lead/Case/Custom objects
  multiplies the API spend.

## 4. Outage detection & recovery

Salesforce publishes status at https://status.salesforce.com.
Note the org's instance (NA45, EU17, etc.) — outages are usually
instance-scoped.

Common outage signatures:

- `503` from `<instance>.my.salesforce.com/services/data/*`.
- `INVALID_SESSION_ID` if the org-wide session pool is reset
  (forces a re-auth via refresh-token).

Recovery:

1. Confirm via status.salesforce.com for the matching instance.
2. The delta poll resumes from `SystemModstamp >= <cursor>`.
3. After major outages, re-verify the connected app is still in
   the org's allowed list.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `[{"errorCode":"INVALID_SESSION_ID"}]` | Token expired or revoked | Refresh access token; §2 routine rotation |
| `403` | `[{"errorCode":"REQUEST_LIMIT_EXCEEDED"}]` | 24h API bucket exhausted | §3; pause backfill until reset |
| `403` | `[{"errorCode":"INSUFFICIENT_ACCESS_OR_READONLY"}]` | Profile lost SObject access | Grant `View All Data` or per-object Read |
| `400` | `[{"errorCode":"MALFORMED_QUERY"}]` | SOQL field changed (schema migration on Salesforce side) | Adjust connector config; re-run validation |
| `503` | `[{"errorCode":"SERVER_UNAVAILABLE"}]` | Per-org transaction throttle | §3; honour `Retry-After` |
