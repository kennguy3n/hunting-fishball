# Trello runbook (`trello`)

> Source: [`internal/connector/trello/trello.go`](../../internal/connector/trello/trello.go)
> · API base: `https://api.trello.com`
> · Delta cursor: `/1/boards/<boardId>/actions?since=<ISO8601>`
>   filtered to `updateCard,createCard` actions

## 1. Auth model

Trello uses its classic API-key + token model: the API key
identifies the integration, and the token (issued by a
Trello user via `/1/authorize`) authorizes access to that
user's boards. Both are carried as query parameters on every
request, never as headers.

```json
{
  "api_key":  "...",
  "token":    "...",
  "board_id": "..."
}
```

Query: `?key=<api_key>&token=<token>`.

## 2. Credential rotation

Routine rotation:

1. **https://trello.com/power-ups/admin** → select the
   Power-Up → **API Key & Secrets** → **Generate a new secret**
   (the secret is the long-lived issuer of tokens; the API
   key itself rotates rarely).
2. Re-issue user tokens for any human-authorized integration
   user via `/1/authorize?response_type=token&...&expiration=never`.
3. Update the secret-manager record with the new
   `api_key` + `token` pair.
4. Verify with `GET /1/boards/<board_id>?key=...&token=...` —
   must return `200 OK`.

Emergency rotation:

1. Each Trello user can revoke any token they issued from
   **Settings → Power-Ups and integrations → Account settings
   → Applications**.
2. Pause the source.
3. Use Trello's audit log (Enterprise-only) to enumerate any
   actions taken during the compromise window. Non-Enterprise
   accounts have no per-token audit trail.
4. Re-issue the token with a fresh expiration and the
   narrowest read/write scopes the integration needs.

## 3. Quota / rate-limit incidents

Trello enforces both a per-API-key (300 req/10s) and a
per-token (100 req/10s) limit. Throttle signal:

- `429 Too Many Requests` body
  `API key + token rate limit exceeded` — applies to the
  per-token bucket. The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 8 per
  token (intentionally below 10 to leave headroom).
- The actions endpoint is paginated by `limit` (max 1000) and
  `before=<actionId>` for backwards traversal. The connector's
  delta uses `since=<ISO8601>` which sidesteps action-id
  pagination entirely.

## 4. Outage detection & recovery

Trello publishes status at https://trello.status.atlassian.com
(rolled up under Atlassian's umbrella status page).

Common outage signatures:

- `503 Service Unavailable` during Atlassian platform
  maintenance windows.
- `502 Bad Gateway` from the CDN during certificate rotation.
- `200 OK` with stale data — Trello's read replicas can lag
  during regional failover; this is rare but the connector's
  delta naturally re-pulls on the next tick.

Recovery:

1. Confirm via https://trello.status.atlassian.com.
2. Delta resumes from the persisted `since` ISO8601 cursor.
   Actions are returned newest-first by Trello; the connector
   dedupes by card id within a single page and uses the
   latest action's date as the new cursor.
3. Cards archived (`closed=true`) during the outage are still
   returned by the cards list; deletions show up as
   `archiveCard` actions which the connector currently treats
   as upserts (the card body is preserved).

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `400` | `invalid id` | Board id no longer exists or token user lost access | Update `board_id` or re-authorize |
| `401` | `unauthorized permission requested` | Token scope insufficient (e.g. read but board is private) | Re-issue token with `read,write` |
| `404` | `not found` | Board archived (Trello soft-archives boards but the API treats them as 404 for non-members) | Verify board state |
| `429` | `API key + token rate limit exceeded` | Per-token throttle | §3 |
| `500` | `Trello server error` | Trello side, transient | Retry with backoff |
| `503` | `Service Unavailable` | Atlassian-platform maintenance | §4 |
