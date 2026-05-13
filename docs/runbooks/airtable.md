# Airtable runbook (`airtable`)

> Source: [`internal/connector/airtable/airtable.go`](../../internal/connector/airtable/airtable.go)
> · API base: `https://api.airtable.com`
> · Delta cursor: `filterByFormula=IS_AFTER(LAST_MODIFIED_TIME(),'<cursor>')`
>   on `/v0/<baseId>/<tableIdOrName>`

## 1. Auth model

Airtable's REST API authenticates via a personal access token
(PAT) or an OAuth integration token. PATs are scoped to one or
more bases and a fixed set of scopes (e.g.
`data.records:read`, `schema.bases:read`).

```json
{
  "access_token": "patXXXX...",
  "base_id":      "appXXXX...",
  "table":        "Projects"
}
```

Header: `Authorization: Bearer <access_token>`.

## 2. Credential rotation

Routine rotation:

1. **Account → Developer hub → Personal access tokens** →
   **Create token** with the same scopes + base access as the
   existing token.
2. Update the secret-manager record.
3. Verify with `GET /v0/<baseId>/<tableIdOrName>?maxRecords=1`
   — must return `200 OK`.
4. **Personal access tokens** → revoke the previous token.

Emergency rotation:

1. **Personal access tokens** → revoke the leaked token
   immediately.
2. Pause the source.
3. Airtable does not expose per-token audit history — assume
   any base accessible to the token was readable during the
   compromise window. If sensitive data was in scope, review
   the base's revision history (per-record) for unexpected
   writes.
4. Issue a replacement token with the narrowest scope set the
   integration actually needs.

## 3. Quota / rate-limit incidents

Airtable enforces a per-base 5 req/sec hard limit. Throttle
signal:

- `429 Too Many Requests` with a 30s lockout — after a 429,
  the base is locked for 30 seconds regardless of subsequent
  request rate. The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 3
  (intentionally below 5 to avoid bursting into the lockout).
- `pageSize=100` is the maximum — `offset` pagination via the
  response's `offset` field.
- `filterByFormula` adds CPU load on Airtable's side; very
  large bases (1M+ rows) should pre-filter via
  `view=<small_view>` when possible.

## 4. Outage detection & recovery

Airtable publishes status at https://status.airtable.com.

Common outage signatures:

- `503 Service Unavailable` during platform maintenance
  windows (announced via the status page).
- `502 Bad Gateway` from the CDN edge during certificate
  rotation.
- `422 Unprocessable Entity` when the formula references a
  field that was renamed — the connector re-resolves field
  names per page so this self-heals.

Recovery:

1. Confirm via the status page.
2. Delta resumes from the persisted `LAST_MODIFIED_TIME`
   ISO8601 cursor. Records updated mid-outage are captured on
   the next tick.
3. Hard-deleted records do not appear in subsequent delta
   responses — the connector cannot emit `ChangeDeleted` for
   Airtable. Use record sync or full re-list for deletions.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Authentication required` | Token revoked | §2 |
| `403` | `Not authorized` | Token's scope doesn't include this base | Re-issue token with `data.records:read` for this base |
| `404` | `Could not find table` | Table renamed or PAT revoked from base | Update the `table` field or re-grant base access |
| `422` | `INVALID_FILTER_BY_FORMULA` | Field referenced by formula was renamed | Self-heals on next tick — no action |
| `429` | `Rate limit exceeded` | Per-base 5 req/sec | §3 — back off 30s |
| `503` | `Service Unavailable` | Platform maintenance | §4 |
