# Sitemap crawl runbook (`sitemap`)

> Source: [`internal/connector/sitemap/sitemap.go`](../../internal/connector/sitemap/sitemap.go)
> · Input: one or more `sitemap.xml` URLs (XML
>   `<urlset>` / `<sitemapindex>` per
>   https://www.sitemaps.org/protocol.html).
> · Delta cursor: `<lastmod>` timestamp (RFC3339,
>   RFC3339Nano, `2006-01-02T15:04:05`, or `2006-01-02`).

## 1. Auth model

By default no auth — sitemaps are typically public. The
connector optionally accepts HTTP basic auth (intranet
sitemaps) or a Bearer token for sitemaps gated behind a CDN's
edge-auth (e.g. Cloudflare Access service tokens).

```json
{ "sitemap_urls": ["https://example.com/sitemap.xml"] }
```

Optional:

```json
{
  "sitemap_urls": ["https://intranet.example.com/sitemap.xml"],
  "username": "ingest-bot",
  "password": "..."
}
```

or

```json
{
  "sitemap_urls": ["..."],
  "bearer_token": "cf-token..."
}
```

## 2. Credential rotation

Routine rotation (only if `username`/`password` or
`bearer_token` are set):

1. Generate the new credential at the gating system (Cloudflare
   Access service token, on-prem IdP, etc.).
2. Update the secret-manager record on the source.
3. Verify: `GET <first sitemap URL>` returns `200 OK` and the
   parser yields ≥ 1 entry.
4. Revoke the previous credential.

Emergency rotation:

1. Revoke the leaked credential at the gating system.
2. Pause every sitemap source bound to that credential.
3. Audit access logs at the gating system (Cloudflare Access
   logs / IdP audit) for traffic in the leak window.
4. If the sitemap exposed unintended URLs, also rotate the
   underlying secret on each origin URL referenced by the
   sitemap.

## 3. Quota / rate-limit incidents

Origin behaviour varies — most sites tolerate sustained
crawling at 1 rps. The connector wraps `429 Too Many Requests`
from either the sitemap host or any followed `<loc>` URL to
[`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` floor to 1 for
  public sites, 5 for intranets.
- Override the `User-Agent` per source if the origin blocks
  the default `Go-http-client/1.1` UA.

## 4. Outage detection & recovery

Sitemap origins don't have a centralised status page; outages
manifest as:

- `503 Service Unavailable` / `502 Bad Gateway` from the
  sitemap host during origin maintenance.
- Sudden truncation of `<urlset>` (fewer entries than the
  previous crawl) when the origin partially returns stale
  cache.

Recovery:

1. Open the sitemap URL in a browser to confirm it returns
   well-formed XML.
2. Delta poll resumes from the persisted `<lastmod>`
   timestamp — entries with `<lastmod>` ≤ cursor are skipped.
3. URLs absent from the sitemap on a subsequent crawl are NOT
   automatically deleted (sitemaps don't carry tombstones).
   Configure a periodic full-crawl reconciliation to emit
   `ChangeDeleted` for the gone URLs.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `Unauthorized` | Basic auth / Bearer revoked | §2 routine rotation |
| `403` | `Forbidden` | UA / IP blocked by origin's WAF | Whitelist Devin's egress IPs |
| `404` | `Not Found` | Sitemap URL relocated | Update source config |
| `429` | `Too Many Requests` | Origin throttle | §3 |
| `503` | n/a | Origin maintenance | §4 |
| `parse error` | n/a | Malformed XML (e.g. CDATA missing closing tag) | File a defect with the origin's webmaster |
