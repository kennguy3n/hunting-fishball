# GitHub runbook (`github`)

> Source: [`internal/connector/github/github.go`](../../internal/connector/github/github.go)
> · API base: `https://api.github.com`
> · Webhook: GitHub webhooks (`WebhookPath() = "/github"`)
> · Delta cursor: `since=<rfc3339>` filter on commits / issues

## 1. Auth model

GitHub personal access token (PAT) or fine-grained token. The
credential blob:

```json
{
  "access_token": "ghp_..."
}
```

The connector adds:

- `Authorization: Bearer ghp_...`
- `Accept: application/vnd.github+json`
- `X-GitHub-Api-Version: 2022-11-28`

For org-wide ingestion, prefer a **GitHub App** installation
token over a PAT. The credential class accepts either format —
both encode as Bearer. Required scopes for a PAT:

- `repo` (read repository contents and issues)
- `read:org` (list installations / repositories)

Webhook delivery is verified via the standard
`X-Hub-Signature-256: sha256=<hex>` header against a shared
secret. `HandleWebhook` extracts the relevant
`pusher`/`pull_request`/`issue` fields and emits the matching
`DocumentChange`.

## 2. Credential rotation

Routine rotation:

1. <https://github.com/settings/tokens?type=beta> (fine-grained)
   or `/settings/tokens` (classic) → "Generate new token" with
   the same scopes.
2. PATCH the source credential via the admin portal.
3. Verify: `GET /user` → `{"login":"<bot-account>"}`.
4. Revoke the previous token from the same GitHub settings page.

GitHub App rotation:

1. App settings → "Generate a new private key" (.pem). The
   platform credential service signs short-lived JWTs with this
   key to mint installation access tokens.
2. PATCH the encrypted .pem blob via the admin portal.
3. Old keys remain valid until explicitly deleted; remove them
   after one full poll cycle has used the new key.

Webhook secret rotation:

1. Generate a new random secret.
2. Update both the platform side (admin portal) and the GitHub
   webhook configuration in the same maintenance window so old
   signed payloads stop verifying.

## 3. Quota / rate-limit incidents

GitHub PATs: 5 000 requests / hour. GitHub Apps: 5 000 / hour /
installation. Symptoms:

- `403 Forbidden` with `X-RateLimit-Remaining: 0` and
  `X-RateLimit-Reset: <unix>`.
- The connector treats `X-RateLimit-Remaining: 0` as a `429`
  equivalent; the platform-side retry budget waits until
  `X-RateLimit-Reset`.

Knobs:

- Per-source RPS via the Redis token bucket. With
  the 5 000 / hour cap, ~1.4 RPS sustained is the safe ceiling.
- For org-wide ingestion, prefer GitHub App installation tokens
  (they get separate rate buckets per installation rather than
  sharing a single PAT).

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- Webhook 5xx delivery failures (GitHub retries 8 times over 8
  hours, then drops the delivery).
- `pipeline.fetch` span error rate spike.

Recovery:

1. Confirm via <https://www.githubstatus.com>.
2. After webhook delivery is exhausted, force a delta poll from
   the admin portal — the connector replays
   `GET /repos/.../commits?since=<last-success>` and ingests
   anything that slipped.
3. Use GitHub's "Recent Deliveries" UI in the webhook config to
   manually replay deliveries from the lookback window.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401 Unauthorized` | `{"message":"Bad credentials"}` | Token rotated / revoked | §2 rotation |
| `403 Forbidden` | `{"message":"...rate limit..."}` | Rate-limit exhausted | §3; wait until reset |
| `403 Forbidden` | `{"message":"Resource not accessible by integration"}` | App scope removed | Re-grant scope in installation settings |
| `404 Not Found` | n/a | Repo deleted / made private | Emit `ChangeDeleted` |
| `422 Unprocessable Entity` | `{"message":"Validation Failed"}` | Bad query | Reset cursor in admin portal |
| `502/503/504` | n/a | GitHub backend hiccup | Coordinator retry |
| `403` on webhook verification | n/a | `X-Hub-Signature-256` mismatch | Rotate webhook secret |
