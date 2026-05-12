# Monday.com runbook (`monday`)

> Source: [`internal/connector/monday/monday.go`](../../internal/connector/monday/monday.go)
> · API base: `https://api.monday.com/v2`
> · Webhook: Monday.com webhooks (planned for a later round)
> · Delta cursor: GraphQL `items_page_by_column_values` on
>   `__last_updated__` ≥ cursor

## 1. Auth model

Personal API token (raw value in `Authorization`).

```json
{ "api_token": "eyJhbGciOi..." }
```

Header: `Authorization: eyJhbGciOi...`.

Required role: the user behind the token needs **member** access
to every Board the source surfaces — viewer-only tokens cannot
list items.

## 2. Credential rotation

Routine rotation:

1. Monday.com → **Avatar** → **Administration** → **API** →
   **Generate new token**.
2. PATCH the source credential via the admin portal.
3. Verify: `query { me { id } }` returns the bot user's id.
4. Revoke the previous token in the same panel.

Emergency rotation:

1. Workspace admin → **Administration** → **API** → revoke the
   leaked token.
2. Pause the source.
3. Generate a fresh token and patch the credential.
4. Audit Monday.com's **Admin Activity Log** for any mutations
   during the leak window.

## 3. Quota / rate-limit incidents

Monday.com enforces a *complexity budget* (10M per minute per
account). Each GraphQL query consumes complexity proportional to
its fan-out; the rate-limit signal arrives via the GraphQL error
envelope, not via HTTP 429.

Throttle signals:

- HTTP 429 with `Retry-After` header (rare; only on the
  REST-shaped endpoints we do not use).
- HTTP 200 with `{"errors":[{"extensions":{"code":"ComplexityException"}}]}`
  — the connector wraps this to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).
- HTTP 200 with `{"errors":[{"extensions":{"status_code":429}}]}`
  — same wrap.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 2.
- Reduce per-board `items_page(limit:)` (currently 100) to lower
  per-query complexity.
- Filter the board namespace list to the boards customers care
  about — the items-by-column query is the dominant complexity
  cost.

## 4. Outage detection & recovery

Monday.com publishes status at https://status.monday.com.

Common outage signatures:

- `502 Bad Gateway` from `/v2` during Monday.com deploys.
- HTTP 200 with `{"error_code":"InternalServerError"}` envelope
  during a backend incident — the connector surfaces this as a
  generic error and the iterator retries.

Recovery:

1. Confirm via the status page.
2. Delta poll resumes from the last persisted RFC3339 timestamp.
3. For prolonged outages, complexity budgets often spike post-
   recovery — the adaptive rate limiter will throttle naturally.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `200` | `{"errors":[{"extensions":{"code":"ComplexityException"}}]}` | Complexity budget exceeded | §3 |
| `200` | `{"errors":[{"extensions":{"status_code":429}}]}` | Rate limit | §3 |
| `401` | `{"errors":[{"message":"Not Authenticated"}]}` | Token revoked | §2 routine rotation |
| `403` | `{"errors":[{"message":"Forbidden"}]}` | User lost board access | Re-invite the user |
| `200` | `{"errors":[{"message":"Board not found"}]}` | Board deleted | Remove the namespace from the source |
| `502` | n/a | Upstream incident | §4 |
