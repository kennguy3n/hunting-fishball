# KChat runbook (`kchat`)

> Source: [`internal/connector/kchat/kchat.go`](../../internal/connector/kchat/kchat.go)
> · API base: `https://kchat.internal/api/v1` (configurable per tenant)
> · Webhook: KChat Events API (`WebhookPath() = "/kchat"`)
> · Delta cursor: `/channels.changes?cursor=...`

## 1. Auth model

KChat is the internal-tenant chat platform. The connector
authenticates with a per-workspace API token (signed by the
KChat admin plane) plus an optional HMAC signing secret used to
verify webhook payloads.

```json
{
  "api_token": "kc-...",
  "signing_secret": "<for webhook signature verification>"
}
```

Header layout:

- `Authorization: Bearer kc-...`
- `Content-Type: application/json` on writes; `Accept:
  application/json` on reads.

Webhook payloads carry `X-KChat-Signature` and
`X-KChat-Request-Timestamp`; `VerifyWebhookRequest` rejects any
request with a stale timestamp or mismatched HMAC.

## 2. Credential rotation

Routine rotation:

1. KChat admin → **Integrations** → hunting-fishball app →
   **Reissue token** to mint a fresh `kc-...`.
2. PATCH the source credential via the admin portal; the platform
   re-encrypts and persists.
3. Verify: `GET /api/v1/users.me` should return the bot identity.
4. The webhook subscription URL is unchanged; KChat keeps the
   same signing secret unless explicitly rotated.

Emergency rotation (token leak):

1. KChat admin → **Tokens** → revoke the leaked `kc-...`
   immediately.
2. Pause the source in the admin portal.
3. Reissue the token + signing secret atomically (both halves at
   the same time) to close the replay window.
4. Update both `api_token` and `signing_secret` in the credential
   blob before resuming ingestion.

## 3. Quota / rate-limit incidents

KChat enforces a per-tenant token bucket: 60 requests / minute /
token by default. Endpoints touched:
`/users.me`, `/channels.list`, `/channels.history`,
`/messages.get`, `/channels.changes`.

KChat surfaces throttling as:

- `429 Too Many Requests` with optional `Retry-After`.
- Connector wraps the response with
  [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- The platform-side rate limiter
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go))
  enforces a per-source RPS via Redis token bucket. Tune
  `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` below 1.0 to stay
  inside the default per-token bucket.
- Backfilling a large workspace? Run the initial sync from a
  dedicated bot token so the live token's quota is preserved.

## 4. Outage detection & recovery

Operational signal: the ingest dashboard's `kchat` panel reports
`connector_fetch_errors_total` rising while
`connector_documents_total` plateaus. Cross-check the KChat
status page (https://status.kchat.internal) for a confirmed
incident.

Common outage signatures:

- `503 Service Unavailable` from `kchat.internal/api/*`.
- Webhook deliveries paused (KChat retries for up to 6h).

Recovery:

1. Confirm the upstream outage via the status page.
2. Once KChat recovers, force a delta poll: admin portal →
   "Re-sync now" calls `DeltaSync` with the last cursor.
3. If the cursor has aged past KChat's retention window (default
   30d), the platform falls back to a full `ListDocuments` walk;
   monitor `delta_full_resync_total` for the source.
4. Re-verify webhook signature delivery after recovery — KChat
   may rotate the signing secret server-side after a security
   incident.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"error":"invalid_token"}` | Token revoked | §2 routine rotation |
| `403` | `{"error":"forbidden_channel"}` | Bot not in channel | Invite bot via KChat admin → Channels |
| `404` | `{"error":"channel_not_found"}` | Channel deleted | Remove from namespace filter; let next delta drop it |
| `429` | n/a | Per-token bucket exhausted | §3; honour `Retry-After`; lower per-source RPS |
| `409` | `{"error":"stale_cursor"}` | Delta cursor expired | Trigger full re-sync; clear stored cursor |
| `503` | n/a | Upstream outage | §4 |
| webhook `signature_mismatch` | n/a | Replay or rotated secret | §2 emergency flow |
