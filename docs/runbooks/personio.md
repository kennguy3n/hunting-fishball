# Personio runbook (`personio`)

> Source: [`internal/connector/personio/personio.go`](../../internal/connector/personio/personio.go)
> · API base: `https://api.personio.de/v1`
> · Delta cursor: `updated_from=<RFC3339>` filter on
>   `/company/employees`

## 1. Auth model

Personio uses OAuth 2.0 client credentials. The connector
accepts a `client_id` + `client_secret` pair and exchanges them
for a short-lived Bearer at `/auth`.

```json
{ "client_id": "client-...", "client_secret": "secret-..." }
```

The bearer token is then sent as `Authorization: Bearer <token>`.

## 2. Credential rotation

Routine rotation:

1. **Personio → Settings → Integrations → API credentials →
   Generate new credentials** (issues a fresh `client_id` +
   `client_secret`).
2. Update the secret-manager record.
3. Verify: `POST /auth?client_id=...&client_secret=...`
   returns `{"success":true,"data":{"token":"..."}}`.
4. **API credentials** → revoke the previous credential pair.

Emergency rotation:

1. **API credentials** → revoke the leaked credential pair.
2. Pause every Personio source.
3. Open a support ticket with Personio for the API audit log
   of the leaked credential (Personio Pro plans only).
4. If the credential was scoped to a privileged employee
   attribute set (e.g. salary, SSN), notify the data-privacy
   officer per GDPR Art. 33 procedure.

## 3. Quota / rate-limit incidents

Personio applies a per-credential rate limit (~10 rps sustained,
20 rps burst). Throttle signal:

- `429 Too Many Requests` with `Retry-After`.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).
- The `/auth` exchange itself is rate-limited separately —
  burst auth requests will return `429` even when employee
  reads are within budget.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 5.
- Cache the Bearer for its full TTL (Personio issues 24-hour
  tokens) to avoid hitting the `/auth` cap.
- Use `limit=200` (Personio's maximum page size).

## 4. Outage detection & recovery

Personio publishes status at https://www.personio.com/status
(or via the in-product banner for paying customers).

Common outage signatures:

- `503` from `/company/employees` during EU regional failovers.
  Personio's data plane is EU-only, so US-East outages of
  shared transit infrastructure can ripple into Personio.
- `401 invalid_grant` from `/auth` if the credential was
  silently revoked by an admin.

Recovery:

1. Confirm via the status page.
2. Delta poll resumes from the persisted `updated_from`
   timestamp. Personio's `updated_at` is monotonically
   non-decreasing per employee.
3. Inactive employees (`status="inactive"`) are emitted as
   `ChangeDeleted`.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `invalid_grant` | Credential revoked | §2 routine rotation |
| `403` | `Access denied` | Credential lacks the **HR Reader** scope | Re-issue with broader scope |
| `404` | `Employee not found` | Hard-deleted | Iterator skips |
| `429` | `Rate limit exceeded` | Per-credential throttle | §3 |
| `503` | n/a | Regional failover | §4 |
