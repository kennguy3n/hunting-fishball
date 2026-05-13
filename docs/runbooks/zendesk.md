# Zendesk runbook (`zendesk`)

> Source: [`internal/connector/zendesk/zendesk.go`](../../internal/connector/zendesk/zendesk.go)
> · API base: `https://<subdomain>.zendesk.com/api/v2`
> · Delta cursor: `incremental/tickets.json?start_time=<unix>` watermark

## 1. Auth model

Zendesk Support REST API authenticates either via email +
API token (basic auth, `email/token`) or an OAuth bearer
access token.

```json
{
  "subdomain": "acme",
  "email": "ops@acme.com",
  "api_token": "..."
}
```

Header: `Authorization: Basic base64(<email>/token:<api_token>)`
or `Authorization: Bearer <oauth_token>`.

## 2. Credential rotation

Routine rotation:

1. **Admin Center → Apps and integrations → APIs → Zendesk API
   → Settings → Add API token** (issue a new token bound to
   the same integration user).
2. Update the secret-manager record.
3. Verify with `GET /api/v2/users/me.json` — must return
   `200 OK` with `user.role = "admin"`.
4. Revoke the previous token from the same screen.

Emergency rotation:

1. **API token list** → revoke the leaked token immediately.
2. Pause the source via the admin API.
3. Use the [Audit Log](https://support.zendesk.com/hc/en-us/articles/4408833181722)
   to enumerate any actions the token took during the
   compromise window.
4. Issue a replacement token; if the integration user has
   elevated permissions, right-size before re-enabling.

## 3. Quota / rate-limit incidents

Zendesk enforces per-account per-minute and per-endpoint
limits (e.g. 200 req/min for the Tickets endpoint on Suite).
Throttle signal:

- `429 Too Many Requests` with a `Retry-After` header in
  seconds.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go)
  and `Retry-After` is honored by the adaptive limiter.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` — floor to 2 for
  Tickets, 5 for the incremental export endpoint.
- The incremental export is recommended over the standard
  Tickets list for backfills — it has a higher per-page limit
  (1000 vs. 100) and dedicated quota.

## 4. Outage detection & recovery

Zendesk publishes status at https://status.zendesk.com. Pod
outages are regional (pod1, pod2, etc.); your account's pod
is in the JWT issued by the SSO flow.

Common outage signatures:

- `503 Service Unavailable` during pod failover.
- `503` with a maintenance HTML body when Zendesk runs a
  scheduled global migration.
- `502` from the CDN edge during certificate rotation.

Recovery:

1. Confirm via the status page; note the affected pod.
2. Delta resumes from the persisted `start_time` unix
   timestamp — Zendesk guarantees the incremental export is
   exactly-once if the cursor monotonically advances.
3. Tickets archived during the outage window are surfaced as
   `ChangeUpserted` on the next delta tick.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Couldn't authenticate you` | Token rotated/revoked or user disabled | §2 |
| `403` | `Forbidden` | Integration user lacks the required permission for the ticket's brand | Grant role on the brand |
| `404` | `Ticket not found` | Hard-deleted via Suite admin tools | Iterator skips; delta emits `ChangeDeleted` |
| `422` | `Validation failed` | Custom field schema drift | Refresh the ticket form, replay |
| `429` | `Too Many Requests` | Per-endpoint throttle | §3, honor `Retry-After` |
| `503` | `Service Unavailable` | Pod failover or maintenance | §4 |
