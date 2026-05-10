# Confluence Cloud runbook (`confluence`)

> Source: [`internal/connector/confluence/confluence.go`](../../internal/connector/confluence/confluence.go)
> · API base: `<site_url>/wiki/rest/api`
> · Webhook: not used (poll only)
> · Delta cursor: CQL `lastModified` filter on `/content/search`

## 1. Auth model

Atlassian email + API token, sent as HTTP Basic. The credential
blob:

```json
{
  "email": "ops@acme.com",
  "api_token": "ATATT3...",
  "site_url": "https://acme.atlassian.net"
}
```

`Validate` rejects an empty `email` or `api_token`. The connector
encodes `Authorization: Basic <base64(email:api_token)>` on every
call.

## 2. Credential rotation

Routine rotation:

1. <https://id.atlassian.com/manage-profile/security/api-tokens>
   → "Create API token" with a descriptive label
   (`hf-confluence-<env>-<date>`).
2. PATCH the source credential via the admin portal.
3. Verify: `GET /wiki/rest/api/space?limit=1` →
   `{"results":[...]}`.
4. Revoke the previous API token from the same page once the new
   token is in service for one full poll cycle.

Emergency rotation:

1. From the Atlassian token page, immediately revoke the affected
   token. This invalidates Basic auth across every Atlassian
   product the same email has access to (Jira included).
2. Pause both the Confluence and Jira sources for the same
   `email` to avoid a cascade of 401 retries.
3. Mint a new token, PATCH credentials, resume.

## 3. Quota / rate-limit incidents

Confluence Cloud throttles via:

- `429 Too Many Requests` with `Retry-After` header.
- Atlassian's generic `503 Service Unavailable` during DC
  deploys.

The "Cost" of `/content/search` is higher than `/content/{id}` —
prefer batched delta queries with a tight CQL `lastmodified`
window over wide scans.

Knobs:

- Per-source RPS via the Redis token bucket
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go)).
- The connector caps `limit` at 100 per page; Atlassian rejects
  larger pages.

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- A burst of `503` errors in the Stage 1 fetch span.

Recovery:

1. Confirm via <https://status.atlassian.com>.
2. The CQL cursor is timestamp-based, not opaque, so resumption
   after an outage simply re-runs the same `lastModified > <ts>`
   query — no cursor-reset pathway required.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401 Unauthorized` | `{"message":"Invalid auth"}` | Token rotated / revoked | §2 rotation |
| `403 Forbidden` | `{"message":"User does not have permission"}` | Account lost space access | Re-grant in Confluence UI |
| `404 Not Found` | `{"statusCode":404}` | Page deleted upstream | Emit `ChangeDeleted` |
| `429 Too Many Requests` | n/a | Rate-limit | Honor `Retry-After` |
| `503 Service Unavailable` | n/a | DC deploy / overload | Coordinator retry |
