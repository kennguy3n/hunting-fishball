# BambooHR runbook (`bamboohr`)

> Source: [`internal/connector/bamboohr/bamboohr.go`](../../internal/connector/bamboohr/bamboohr.go)
> · API base: `https://api.bamboohr.com/api/gateway.php/<subdomain>`
> · Delta cursor: `since=<ISO8601>` filter on
>   `/v1/employees/changed`

## 1. Auth model

BambooHR uses HTTP basic auth where **the API key is the
username** and a literal `"x"` is the password. (Putting the
key as the password would leak it into BambooHR's request
logs.)

```json
{ "api_key": "...", "subdomain": "acme" }
```

Header: `Authorization: Basic base64(<api_key>:x)`.

## 2. Credential rotation

Routine rotation:

1. **BambooHR → Account → API Keys → Add API Key** (issue a
   new key bound to the same integration user).
2. Update the secret-manager record.
3. Verify: `GET /v1/employees/directory` returns `200 OK` with
   the `employees` array.
4. **API Keys** → revoke the previous key (BambooHR has no
   key-level audit trail, so always revoke promptly).

Emergency rotation:

1. **API Keys** → revoke the leaked key immediately.
2. Pause the source.
3. Open a support ticket with BambooHR for **API access
   history** for the leaked key (only available via support).
4. If the integration user has elevated permissions (e.g.
   custom-access-level admin), audit and right-scope
   permissions before re-issuing a new key.

## 3. Quota / rate-limit incidents

BambooHR enforces a per-account-per-minute rate limit (varies
by plan; ~1000 requests/min on Pro). Throttle signal:

- `503 Service Unavailable` (BambooHR's idiosyncratic 503 for
  rate-limit; some endpoints also return `429`).
- The connector wraps both `429` and `503` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 8.
- The `/v1/employees/directory` endpoint returns ALL employees
  in a single response — there's no pagination, so call it
  sparingly (delta `/v1/employees/changed?since=...` is the
  preferred refresh path).

## 4. Outage detection & recovery

BambooHR publishes status at https://status.bamboohr.com.

Common outage signatures:

- `503` from the gateway during regional failover (BambooHR is
  US-East/US-West active-active; failovers can briefly bounce
  the gateway).
- `403 Forbidden` from `/v1/employees/<id>` if the integration
  user was downgraded out of the **Employees: full access**
  custom access level.

Recovery:

1. Confirm via the status page.
2. Delta resumes from the persisted `since` ISO8601
   timestamp.
3. BambooHR marks departed employees with `action="Deleted"`
   in the `/v1/employees/changed` payload — the connector
   converts these to `ChangeDeleted`.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Bad credentials` | Key rotated/revoked | §2 routine rotation |
| `403` | `Insufficient permissions` | Custom access level too narrow | Grant **Employees: full access** |
| `404` | `Employee not found` | Hard-deleted | Iterator skips; delta emits `ChangeDeleted` |
| `429` | `Rate limit exceeded` | Per-account throttle | §3 |
| `503` | `Service is unavailable` | Either rate-limit (most common) or genuine outage | §3 / §4 |
