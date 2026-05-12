# BookStack runbook (`bookstack`)

> Source: [`internal/connector/bookstack/bookstack.go`](../../internal/connector/bookstack/bookstack.go)
> · API base: `https://<host>/api`
> · Delta cursor: RFC 3339 timestamp compared against each
>   page's `updated_at` (results sorted `-updated_at`).

## 1. Auth model

BookStack issues a Token-ID + Token-Secret pair per user.
Auth is a single header constructed from both.

```json
{
  "base_url":     "https://wiki.example.com",
  "token_id":     "...",
  "token_secret": "..."
}
```

Header: `Authorization: Token <token_id>:<token_secret>`.

## 2. Credential rotation

Routine rotation:

1. **BookStack → User profile → API tokens → Create token**
   for the integration user with a future expiry.
2. Update the secret-manager record.
3. Verify: `GET /api/books?count=1` returns 200.
4. **User profile → API tokens → Delete** the prior token.

Emergency rotation:

1. **User profile → API tokens** → delete the leaked token.
2. Pause every BookStack source.
3. Audit BookStack's per-token activity in
   `users/<id>/activities` for unexpected actions.

## 3. Quota / rate-limit incidents

BookStack enforces a Laravel-style request throttler. The
`X-RateLimit-Limit` / `X-RateLimit-Remaining` headers are
returned on every response.

- `429 Too Many Requests` with `Retry-After` (seconds).
- The connector wraps `429` to
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 4 — the
  out-of-the-box BookStack policy is 60 requests/minute per
  IP.
- Self-hosted: bump the throttle in `config/throttle.php` if
  the on-prem deployment serves >1 integration.

## 4. Outage detection & recovery

BookStack is typically self-hosted; outages match the
hosting platform's signals.

Common outage signatures:

- `502 Bad Gateway` from the fronting reverse proxy during
  `php-fpm` restarts.
- `503` during scheduled `php artisan migrate` deployments.

Recovery:

1. Confirm with the on-prem BookStack admin.
2. Resume from the persisted `updated_at` cursor.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":{"message":"No authorization token"}}` | Token revoked / wrong header format | §2 |
| `403` | `{"error":{"message":"Permission denied"}}` | Token user lost role | Restore role |
| `404` | `{"error":{"message":"Item not found"}}` | Page deleted | Iterator skips |
| `429` | `{"error":{"message":"Too Many Attempts"}}` | Throttled | §3 |
| `500` | `{"error":{"message":"Server Error"}}` | App-level failure | §4 |
