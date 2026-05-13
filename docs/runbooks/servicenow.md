# ServiceNow runbook (`servicenow`)

> Source: [`internal/connector/servicenow/servicenow.go`](../../internal/connector/servicenow/servicenow.go)
> · API base: `https://<instance>.service-now.com/api/now`
> · Delta cursor: `sysparm_query=sys_updated_on>...` watermark on
>   each `/api/now/table/<table>` poll

## 1. Auth model

ServiceNow Table API supports HTTP basic auth (recommended for
service accounts) or OAuth 2.0 with a refresh token. Most
production deployments use basic auth against a dedicated
integration user with **Web Service** access only.

```json
{
  "instance": "acmeprod",
  "username": "integration.context",
  "password": "..."
}
```

Header: `Authorization: Basic base64(<username>:<password>)`.

## 2. Credential rotation

Routine rotation:

1. **User Administration → Users** → open the integration user
   → **Set Password**. Avoid sharing the password with humans;
   put it directly into the secret manager.
2. Update the secret-manager record.
3. Verify with `GET /api/now/table/sys_user?sysparm_query=user_name=integration.context&sysparm_limit=1` — must return `200 OK`.
4. Reset login attempt counters if the user was locked out
   during rotation (`Account locked = false`).

Emergency rotation:

1. Disable the user (`Active = false`) — this immediately
   revokes all sessions.
2. Pause the source.
3. Use the **System Logs → Transactions** report filtered by
   `User name = integration.context` to enumerate any actions
   the credential took during the compromise window.
4. Re-enable with a new password and right-size the
   integration user's roles before re-enabling the source.

## 3. Quota / rate-limit incidents

ServiceNow enforces both per-user transaction limits
(**Inbound REST**) and per-instance worker pool exhaustion.
Throttle signal:

- `429 Too Many Requests` with a `Retry-After` header
  (default 60s) when the integration user hits its inbound
  transaction quota.
- `503 Service Unavailable` from the load balancer when the
  semaphore pool is exhausted.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 4 for
  the Table API.
- Page size: `sysparm_limit=200`. Above 200 the response can
  exceed the `glide.json.response.length` ceiling and return
  a truncated payload.

## 4. Outage detection & recovery

ServiceNow publishes status at https://status.servicenow.com.
Instance maintenance windows are announced via the Now
Support portal under **Instance → Schedule**.

Common outage signatures:

- `503` during the weekly clone window (Sat 03:00 UTC for
  most regions).
- `502 Bad Gateway` from the LB during instance restarts.
- `200 OK` with an empty `result` array and `X-RateLimit-Reset`
  header — this is a soft block from the inbound rate limiter
  rather than an outage.

Recovery:

1. Confirm via Now Support → Instance → Schedule.
2. Delta resumes from the persisted `sys_updated_on`
   ServiceNow-format timestamp (`2006-01-02 15:04:05`).
3. Records updated mid-outage that finish after recovery will
   be picked up by the next delta tick — `sys_updated_on` is
   set on commit, not on receive.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `User Not Authenticated` | Password rotated or user disabled | §2 |
| `403` | `User Not Authorized` | Missing **Web Service** role on the table | Grant `soap` + table read ACL |
| `404` | `No Record found` | Record was hard-deleted via update-set rollback | Iterator skips |
| `429` | `Too Many Requests` | Inbound transaction quota hit | §3, honor `Retry-After` |
| `500` | `Internal Server Error` with `glide-` stack trace | ServiceNow scripted REST API bug | File a HI ticket, replay |
| `503` | `Service Unavailable` | Clone/maintenance window | §4 |
