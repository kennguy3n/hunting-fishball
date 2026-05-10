# GitLab runbook (`gitlab`)

> Source: [`internal/connector/gitlab/gitlab.go`](../../internal/connector/gitlab/gitlab.go)
> · API base: `<site_url>/api/v4` (default `https://gitlab.com/api/v4`)
> · Webhook: GitLab webhooks (`WebhookPath() = "/gitlab"`)
> · Delta cursor: `updated_after=<rfc3339>` on issues / MRs

## 1. Auth model

GitLab personal access token (PAT) or project access token. The
credential blob:

```json
{
  "access_token": "glpat-...",
  "site_url": "https://gitlab.com"
}
```

`site_url` defaults to `gitlab.com` when omitted; self-hosted
GitLab installations set it to e.g. `https://gitlab.acme.com`.
Required scopes:

- `read_api`
- `read_repository`

The connector sends `Authorization: Bearer glpat-...` and
`Accept: application/json`. Webhook payloads are verified by the
configured `X-Gitlab-Token` shared secret.

## 2. Credential rotation

Routine rotation:

1. GitLab → User settings (or project / group settings) →
   **Access tokens** → "Add new token" with the same scopes.
2. PATCH the source credential via the admin portal.
3. Verify: `GET /user` → `{"id": <int>, "username": "<bot>"}`.
4. Revoke the previous token from the same access tokens page.

Webhook secret rotation:

1. Generate a fresh secret.
2. Update both the platform side (admin portal) and the GitLab
   webhook configuration in the same maintenance window.

Self-hosted notes:

- GitLab admin can create a "system hook" that bypasses
  per-project hooks. The connector verifies the same
  `X-Gitlab-Token` header for system hooks; rotation is identical.

## 3. Quota / rate-limit incidents

gitlab.com hosted: ~600 requests / minute / user. Self-hosted
defaults to no rate limit, but admins typically configure
`Application settings → Rate limits`.

Symptoms:

- `429 Too Many Requests` with `Retry-After`.
- `503 Service Unavailable` during DC deploys.

Knobs:

- Per-source RPS via the Redis token bucket
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go)).

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- Webhook 5xx delivery failures.
- `pipeline.fetch` span error rate spike.

Recovery:

1. Confirm via <https://status.gitlab.com> (or the self-hosted
   monitoring stack).
2. After webhook exhaustion, force a delta poll from the admin
   portal — the connector replays `updated_after=<last-success>`
   and ingests anything missed.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401 Unauthorized` | `{"message":"401 Unauthorized"}` | Token revoked | §2 rotation |
| `403 Forbidden` | `{"message":"403 Forbidden"}` | Lost project access | Re-grant in GitLab |
| `404 Not Found` | n/a | Project deleted / archived | Emit `ChangeDeleted` |
| `429 Too Many Requests` | n/a | Rate-limit | §3 honor `Retry-After` |
| `502/503/504` | n/a | Backend hiccup | Coordinator retry |
| `403` on webhook verification | n/a | `X-Gitlab-Token` mismatch | Rotate webhook secret |
