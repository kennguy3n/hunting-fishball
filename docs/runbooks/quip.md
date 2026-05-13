# Quip runbook (`quip`)

> Source: [`internal/connector/quip/quip.go`](../../internal/connector/quip/quip.go)
> · API base: `https://platform.quip.com`
> · Delta cursor: `updated_usec` watermark (microseconds since
>   epoch) walked through `/1/threads/recent`

## 1. Auth model

Quip authenticates via a personal access token issued by the
admin console. The token carries the issuing user's permissions
— grant it to a dedicated service account, not a human's
seat.

```json
{
  "access_token": "..."
}
```

Header: `Authorization: Bearer <access_token>`.

## 2. Credential rotation

Routine rotation:

1. **Admin Console → Personal Access Tokens** for the
   integration account → **Generate New Token**.
2. Update the secret-manager record.
3. Verify with `GET /1/users/current` — must return `200 OK`.
4. **Revoke** the prior token in the same panel once the new
   token is verified in production.

Emergency rotation:

1. Disable the integration account (Admin Console → **Users**
   → **Deactivate**). This invalidates every token the
   account ever held.
2. Pause the source.
3. Audit account activity via the Quip admin audit log for
   the compromise window.
4. Re-enable, mint a fresh token, and re-bind.

## 3. Quota / rate-limit incidents

Quip enforces a per-user request budget. Throttle signal:

- `503 Service Unavailable` (Quip's idiosyncratic 503-on-rate-
  limit; the connector treats this as transient).
- `429 Too Many Requests` on the newer endpoints with
  `X-RateLimit-Limit` / `X-RateLimit-Remaining` /
  `X-RateLimit-Reset` headers.
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go)
  so the adaptive limiter and DLQ replay both treat it as
  retriable.

Knobs:

- Quip's documented ceiling is 50 requests/minute/user; the
  default `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` of 1 is
  safe.
- `/1/threads/recent` returns at most 50 threads per call —
  the connector paginates via the `updated_usec` cursor of
  the oldest returned thread.

## 4. Outage detection & recovery

Quip status: https://status.salesforce.com (Quip rolls up
under Salesforce's status page). Salesforce maintenance
windows can cause read-only intervals where `POST` writes
fail but `GET` reads continue.

Common outage signatures:

- `502 Bad Gateway` from the Salesforce edge during release
  rollout.
- `5xx` from `/1/threads/recent` while the Quip index is
  rebuilding. The connector retries; the watermark advances
  only when a clean response arrives.

Recovery:

1. Confirm via https://status.salesforce.com.
2. Delta resumes from the persisted `updated_usec` cursor —
   Quip's iterator is monotonic per-thread so an exactly-once
   contract holds across restarts.
3. Thread renames don't change the thread ID; existing
   indexed documents are upserted in place.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Unauthorized` | Token revoked / account deactivated | §2 |
| `403` | `Forbidden` | Token holder lost access to the thread / folder | Re-share or remove |
| `404` | `Not found` | Thread hard-deleted | Iterator skips |
| `429` | `Too Many Requests` | Per-user quota exhausted | §3 |
| `503` | `Service Unavailable` | Quip rate-limit signal (legacy endpoints) | §3 |
| `500` | `Internal Server Error` | Quip side; usually transient | Retry with backoff |
