# HubSpot runbook (`hubspot`)

> Source: [`internal/connector/hubspot/hubspot.go`](../../internal/connector/hubspot/hubspot.go)
> · API base: `https://api.hubapi.com`
> · Webhook: HubSpot webhooks (planned for a later round)
> · Delta cursor: `hs_lastmodifieddate >= <ms-epoch>` via `/crm/v3/objects/<type>/search`

## 1. Auth model

HubSpot private-app access token.

```json
{ "access_token": "pat-na1-..." }
```

Header: `Authorization: Bearer pat-na1-...`.

Required private-app scopes (set in HubSpot **Settings** →
**Integrations** → **Private Apps**):

- `crm.objects.contacts.read`
- `crm.objects.companies.read`
- `crm.objects.deals.read`
- `crm.objects.tickets.read`

## 2. Credential rotation

Routine rotation:

1. HubSpot → **Settings** → **Integrations** → **Private Apps**
   → app → **Rotate access token**.
2. PATCH the source credential via the admin portal.
3. Verify: `GET /integrations/v1/me` returns the private-app
   identity.

Emergency rotation:

1. **Private Apps** → app → **Delete** (invalidates the token
   immediately).
2. Pause the source.
3. Recreate the private app with the same scopes; install; mint
   a fresh token.
4. Audit HubSpot's **Settings** → **Audit Logs** for any actions
   taken by the private app during the leak window.

## 3. Quota / rate-limit incidents

HubSpot's per-private-app burst limit is 100 req / 10s. Daily
allotment depends on portal tier (250k–1M req/day for Pro+).

The connector hits `/integrations/v1/me`,
`/crm/v3/objects/<type>` (paginated cursor), and
`/crm/v3/objects/<type>/search` for delta.

Throttle signal:

- `429 Too Many Requests` with `Retry-After`.
- `X-HubSpot-RateLimit-Daily-Remaining` header drops to 0 →
  subsequent calls 429.

Wraps to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 5 (matches
  10/s halved); raise for Enterprise tiers.
- Restrict object types via namespace filter (e.g. only contacts
  + companies) to spread the daily bucket.

## 4. Outage detection & recovery

HubSpot publishes status at https://status.hubspot.com.

Common outage signatures:

- `503` from `api.hubapi.com/crm/*`.
- `502 Bad Gateway` during HubSpot deploys.

Recovery:

1. Confirm via status.hubspot.com.
2. Delta poll resumes from the last `hs_lastmodifieddate`.
3. For prolonged outages, monitor the daily quota — HubSpot
   often grants temporary quota relief post-incident.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"status":"error","message":"Authentication credentials..."}` | Token revoked | §2 routine rotation |
| `403` | `{"status":"error","message":"You do not have permission..."}` | Scope missing | Re-grant scope in Private App settings |
| `429` | `{"status":"error","message":"daily limit exceeded"}` | Daily quota exhausted | §3; wait for reset |
| `429` | `{"status":"error","message":"rate limit exceeded"}` | 10s burst exceeded | §3; honour `Retry-After` |
| `500` | n/a | Upstream incident | §4 |
