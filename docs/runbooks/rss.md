# RSS / Atom runbook (`rss`)

> Source: [`internal/connector/rss/rss.go`](../../internal/connector/rss/rss.go)
> · API base: configured per-feed (any HTTPS URL serving
>   `application/rss+xml`, `application/atom+xml`, or `text/xml`)
> · Webhook: not applicable (poll-only)
> · Delta cursor: max `<pubDate>` / `<updated>` of the previous
>   poll (RFC3339)

## 1. Auth model

Most public feeds require no credentials. The connector accepts
optional basic-auth or a static bearer token for feeds that gate
access (e.g. internal corp planet feeds, paid newsletters).

```json
{ "feed_url": "https://blog.example.com/feed",
  "auth": { "type": "basic", "username": "u", "password": "p" } }
```

Or:

```json
{ "feed_url": "https://newsletter.example.com/feed",
  "auth": { "type": "bearer", "token": "rss-..." } }
```

Required scopes: feed-author / publisher decides — typically a
read-only token.

## 2. Credential rotation

Routine rotation (when the feed requires auth):

1. Publisher → newsletter platform → rotate the access token.
2. PATCH the source credential via the admin portal.
3. Verify: `curl -I <feed_url>` returns 200.

Emergency rotation:

1. Pause the source.
2. Coordinate with the publisher to revoke the leaked secret
   (out-of-band, e.g. email the platform's support).
3. Re-issue and patch the credential.

For public feeds: no credential to rotate; if the feed URL is
leaked, change the feed path in the source config and ask the
publisher to invalidate the old URL.

## 3. Quota / rate-limit incidents

RSS publishers do not advertise a quota. Norms:

- Most feeds tolerate a 5-minute poll cadence.
- Some publishers serve 429 / 503 when polled more aggressively.
- A small minority enforce HTTP cache validation
  (`If-Modified-Since` / `ETag`) and 304-return cached responses —
  the connector honours `Last-Modified` when present.

Throttle signal:

- `429 Too Many Requests` with `Retry-After`.
- `503 Service Unavailable` from publishers that load-shed under
  burst.

Wraps to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` default 0.0033
  (one request per 5 minutes).
- Per-source `delta_min_interval_seconds` lifts the poll cadence
  if a publisher complains.

## 4. Outage detection & recovery

The publisher's status page (if any) is the source of truth.

Common outage signatures:

- `404 Not Found` — feed moved or removed; the source operator
  must update the URL.
- `503` for sustained periods — publisher rate-limiting or DNS
  drift; investigate via `dig <host>`.
- Feed body returns HTML (e.g. a Cloudflare interstitial) instead
  of XML — the parser errors out; the iterator surfaces the
  parse failure.

Recovery:

1. Confirm via `curl -I <feed_url>` and `dig <host>`.
2. Delta poll resumes from the last persisted RFC3339 timestamp.
3. For 404, update the feed URL and re-bootstrap.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `200` | `<html>...</html>` (not XML) | Cloudflare / WAF interstitial | Allowlist the source IP or switch User-Agent |
| `301` / `302` | n/a | Feed URL moved | Update the source config |
| `304` | empty | Not modified since `If-Modified-Since` | Iterator skips |
| `401` | n/a | Auth required / token expired | §2 routine rotation |
| `404` | n/a | Feed removed | §4; update the URL |
| `429` | n/a | Burst exceeded | §3; reduce poll cadence |
| `503` | n/a | Publisher overload | §4 |
