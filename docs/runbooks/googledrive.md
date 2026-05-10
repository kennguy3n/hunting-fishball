# Google Drive runbook (`google_drive`)

> Source: [`internal/connector/googledrive/googledrive.go`](../../internal/connector/googledrive/googledrive.go)
> · API base: `https://www.googleapis.com/drive/v3`
> · Webhook: not used (poll only)
> · Delta cursor: `changes.list` page token

## 1. Auth model

OAuth 2 access token issued for a Google Workspace user or service
account with at least the `drive.readonly` scope. The credential
blob the connector decrypts in `Connect` has the shape:

```json
{
  "access_token": "ya29....",
  "refresh_token": "1//0g...",
  "client_id": "<oauth-client>.apps.googleusercontent.com",
  "client_secret": "GOCSPX-...",
  "token_uri": "https://oauth2.googleapis.com/token"
}
```

`Validate` rejects a credential whose `access_token` is empty;
refresh-token rotation is handled by the platform's credential
service before bytes reach the connector. The HTTP transport adds
`Authorization: Bearer <access_token>` to every call (`do` helper).

## 2. Credential rotation

Routine rotation (no expected outage):

1. In Google Cloud Console → APIs & Services → **Credentials**,
   create a new OAuth client (or rotate the secret on the existing
   one).
2. Re-run the platform admin portal's "Re-authorize" flow for the
   affected source. The flow exchanges the new authorization code
   for a fresh `access_token` + `refresh_token` pair via
   `token_uri`.
3. The admin handler
   ([`internal/admin/source_handler.go`](../../internal/admin/source_handler.go))
   PATCHes the encrypted credential blob in place. The
   forget-worker will *not* fire — credential rotation does not
   alter the source's identity.
4. Watch `internal/admin/health.go` for a fresh `last_success_at`
   on the next poll cycle (≤ 60 s by default).

Emergency rotation (suspected token leak):

1. Revoke the offending access token at
   `https://accounts.google.com/o/oauth2/revoke?token=<token>`.
2. Pause the source in the admin portal — the rate limiter blocks
   any in-flight retries from hitting Drive.
3. Re-authorize per the routine flow, then resume the source.
4. File a `source.purged` audit event if the leak forces a full
   re-ingest; the forget-worker will drop derived chunks before
   the new sync resumes.

## 3. Quota / rate-limit incidents

Drive responds with HTTP `403` with `reason=userRateLimitExceeded`
or `quotaExceeded`, plus `Retry-After` on `429`. The connector
surfaces these as `box: list status=403` style errors (translated
through the upstream `do` helper) and the platform-side
[`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go)
Redis token bucket clamps the next call.

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` — per-source token
  bucket fill rate.
- The connector's `http.Client` carries a 30 s timeout; do **not**
  raise this above 60 s — Drive's `changes.list` and
  `files.export` calls can hang and back-pressure the pipeline.

When the per-user quota is exhausted (typical: 10 000 req / 100 s
/ user), pause the source for the remainder of the bucket window
and let the rate limiter drain. Drive does not surface a residual
quota counter; the only signal is the 429 → 200 transition.

## 4. Outage detection & recovery

Symptoms:

- `internal/admin/health.go` reports `last_success_at` older than
  `Thresholds.Stale` for the source.
- The Stage 1 fetch span (`pipeline.fetch` in
  [`internal/observability/tracing.go`](../../internal/observability/tracing.go))
  records a non-zero error rate.

Recovery:

1. `GET https://www.googleapis.com/drive/v3/about?fields=user` from
   a control-plane shell to confirm the credential is alive.
2. If the call returns 401/403, jump to §2 (rotation).
3. If the call succeeds but ingestion is still stalled, check
   Kafka consumer lag for `context-engine-ingest`
   ([`deploy/hpa-ingest.yaml`](../../deploy/hpa-ingest.yaml) HPA
   target). Lag spikes here mean the upstream is healthy but the
   pipeline is back-pressured downstream.
4. Drain the source: pause → wait until `health` reports zero
   in-flight → resume. The platform commits the Kafka offset
   per partition only after Stage 4 completes
   ([`internal/pipeline/consumer.go`](../../internal/pipeline/consumer.go)),
   so resuming cannot duplicate ingestion.

## 5. Common error codes

| Upstream | Meaning | Action |
|----------|---------|--------|
| `401 Unauthorized` | Access token expired or revoked | §2 routine rotation |
| `403 forbidden`, `reason=insufficientFilePermissions` | The OAuth client lost access to the file | Re-grant in Drive sharing UI; the connector skips the doc on the next pass |
| `403`, `reason=userRateLimitExceeded` | Per-user RPS exceeded | §3; let the rate limiter back off |
| `404 Not Found` on `FetchDocument` | File deleted upstream between list + fetch | Connector emits `ChangeDeleted` on the next `DeltaSync` |
| `429 Too Many Requests` | Global Drive throttle | §3; do *not* retry inside the connector — the pipeline retry budget is in the coordinator |
| `503 Backend Error` | Drive backend hiccup | Coordinator retries with exponential backoff (`internal/pipeline/coordinator.go::runWithRetry`) |
