# Confluence Server / Data Center runbook (`confluence_server`)

> Source: [`internal/connector/confluence_server/confluence_server.go`](../../internal/connector/confluence_server/confluence_server.go)
> · API base: `https://<host>/rest/api`
> · Webhook: Confluence webhooks (planned for a later round)
> · Delta cursor: CQL
>   `space="<key>" AND lastModified > "<RFC3339>"` against
>   `/rest/api/content/search`

## 1. Auth model

Either a Personal Access Token (PAT) — recommended — or basic
auth (username + password / app-password).

```json
{ "base_url": "https://wiki.acme.com",
  "pat": "NDM2..." }
```

Or:

```json
{ "base_url": "https://wiki.acme.com",
  "username": "ingest-bot",
  "password": "..." }
```

Header (PAT): `Authorization: Bearer NDM2...`.
Header (basic): RFC 7617 `Basic <base64(user:pass)>`.

Required permission: **read** on every Space the source surfaces
(check via **Space tools → Permissions** in the Confluence UI).

## 2. Credential rotation

Routine rotation (PAT):

1. Confluence → **Avatar** → **Settings** → **Personal Access
   Tokens** → **Create token** (issues a new PAT with a
   configurable expiry).
2. PATCH the source credential via the admin portal.
3. Verify: `GET /rest/api/user/current` returns the bot user.
4. **Personal Access Tokens** → revoke the previous token.

Routine rotation (basic auth): rotate the Confluence password or
app-password via the standard account-recovery flow; PATCH the
source.

Emergency rotation:

1. PAT: revoke immediately from **Personal Access Tokens**.
   Basic: change the password via the admin portal.
2. Pause the source.
3. Audit Confluence's **Audit Log** (in DC: **Administration →
   Audit log**) for the leak-window's activity.
4. Re-issue credentials and patch the source.

## 3. Quota / rate-limit incidents

Confluence Server / DC does not impose a documented per-token
rate limit — throttling is operator-controlled via the
`com.atlassian.confluence.api.rest.limit` rate-limit plugin and
the LDAP / database query-budget settings.

Throttle signal:

- `429 Too Many Requests` (when the rate-limit plugin is enabled)
  with `Retry-After` header.
- `503 Service Unavailable` when the database query pool is
  exhausted — the connector wraps this to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 5 (a
  reasonable default for a single-node DC instance).
- Narrow the space namespace list — CQL `lastModified` queries
  against archived spaces are expensive.
- Coordinate with the Confluence operator to raise the rate-limit
  plugin's per-IP allowance.

## 4. Outage detection & recovery

Confluence Server / DC outages are operator-detected. Common
signatures:

- `500 Internal Server Error` from `/rest/api/content/search` —
  CQL grammar issue (the connector logs the query; check syntax)
  or backing database under load.
- `503 Service Unavailable` from `/rest/api/space` during
  reindex / cluster failover.
- `401 Unauthorized` on a known-good PAT — the PAT expired or
  the operator disabled PAT support on the instance.

Recovery:

1. Coordinate with the Confluence admin — check
   `<base>/status` or the JVM heap monitor.
2. Delta poll resumes from the last persisted RFC3339 timestamp.
3. For prolonged outages, the CQL `lastModified` window widens —
   `delta_max_lookback_days` knobs the bound.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"message":"No credentials provided","statusCode":401}` | PAT / basic creds invalid | §2 routine rotation |
| `403` | `{"message":"User not permitted to use Confluence","statusCode":403}` | User lost the space's read permission | Re-grant via **Space tools → Permissions** |
| `404` | `{"message":"No space with key ..."}` | Space deleted / archived | Remove the namespace from the source |
| `429` | n/a (rate-limit plugin response) | Rate limit | §3; honour `Retry-After` |
| `500` | `{"message":"java.lang.IllegalArgumentException"}` | CQL syntax error | Open a bug — should not happen in steady state |
| `503` | n/a | DB / cluster degraded | §4 |
