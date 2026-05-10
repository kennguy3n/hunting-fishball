# Jira Cloud runbook (`jira`)

> Source: [`internal/connector/jira/jira.go`](../../internal/connector/jira/jira.go)
> · API base: `<site_url>/rest/api/3`
> · Webhook: Jira webhooks (`WebhookPath() = "/jira"`)
> · Delta cursor: JQL `updated` filter on `/search`

## 1. Auth model

Atlassian email + API token, sent as HTTP Basic — same shape as
Confluence (the two share the credential class because Atlassian
issues one token for both). The credential blob:

```json
{
  "email": "ops@acme.com",
  "api_token": "ATATT3...",
  "site_url": "https://acme.atlassian.net"
}
```

`Validate` rejects empty `email` or `api_token`. Webhook payloads
are POSTed by Atlassian to `<host>/v1/webhooks/jira` and signed
with a shared secret stored alongside the credential blob (when
configured). The connector's `HandleWebhook` decodes the JSON
event envelope (`webhookEvent`, `issue`) and emits the matching
`DocumentChange`.

## 2. Credential rotation

Identical to [Confluence §2](confluence.md#2-credential-rotation)
because both connectors authenticate against Atlassian Cloud the
same way.

Jira-specific:

- After token rotation, re-run the platform's "Test webhook" flow
  from the admin portal — Jira's webhooks do not embed the API
  token but they do send a header that the connector verifies
  against the shared secret. Rotating the API token is independent
  of the webhook secret.
- To rotate the **webhook secret**, edit the webhook in Jira →
  Settings → System → Webhooks and update the platform-side
  secret in the same admin portal action so old payloads stop
  verifying.

## 3. Quota / rate-limit incidents

Jira Cloud throttles per-user (the email tied to the API token).
Symptoms:

- `429 Too Many Requests` with `Retry-After`.
- `503 Service Unavailable` during deploys.

JQL `updated > ...` queries are inexpensive; `expand=changelog`
is more expensive. The connector defaults to no expand on
`/search` and pulls the full payload on the per-issue
`/issue/{key}` call only when needed.

Knobs:

- Per-source RPS via the Redis token bucket.
- Reduce `pipeline.StageConfig.FetchWorkers` if Jira is throttling
  globally (per-user, not per-source).

## 4. Outage detection & recovery

Detection:

- Stale `last_success_at` in
  [`internal/admin/health.go`](../../internal/admin/health.go).
- Webhook 5xx delivery failures (Jira retries on its end with
  exponential backoff).
- `pipeline.fetch` span error rate spike.

Recovery:

1. Confirm via <https://status.atlassian.com>.
2. After an extended outage where Jira gives up on webhook
   delivery, force a delta poll from the admin portal — the
   connector replays JQL `updated > <last-success>` and ingests
   anything that slipped through.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401 Unauthorized` | `{"errorMessages":["Client must be authenticated"]}` | Token revoked | §2 rotation |
| `403 Forbidden` | `{"errorMessages":["..."]}` | Account lost project access | Re-grant in Jira project settings |
| `404 Not Found` on `/issue/{key}` | n/a | Issue deleted upstream | Emit `ChangeDeleted` |
| `429 Too Many Requests` | n/a | Per-user throttle | §3 honor `Retry-After` |
| `503 Service Unavailable` | n/a | Atlassian deploy / outage | Coordinator retry |
| `400 Bad Request` on JQL | `{"errorMessages":["JQL syntax error"]}` | Cursor JQL malformed (rare) | Reset cursor in admin portal |
| `403` on webhook verification | n/a | Webhook signature mismatch | Rotate webhook secret |
