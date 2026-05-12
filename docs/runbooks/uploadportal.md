# Upload portal runbook (`upload_portal`)

> Source: [`internal/connector/upload_portal/upload_portal.go`](../../internal/connector/upload_portal/upload_portal.go)
> · Endpoint: `POST /v1/webhooks/upload_portal`
> · Delta cursor: none — every document arrives via webhook.

## 1. Auth model

The upload portal is a server-side receiver. Clients POST
multipart bodies to the platform's webhook endpoint with an
HMAC-SHA256 signature header.

```json
{
  "webhook_secret": "...",
  "allowed_mime":   ["application/pdf", "text/markdown"],
  "max_size_bytes": 10485760
}
```

Header: `X-Upload-Signature-256: sha256=<hex>` computed
over the raw request body using `webhook_secret` as the
HMAC key.

## 2. Credential rotation

Routine rotation:

1. Generate a new 32-byte hex secret (`openssl rand -hex 32`).
2. Update the secret-manager record.
3. Issue a fresh batch of pre-signed upload tokens to
   clients (each carries the new HMAC binding).
4. After clients have migrated, retire the prior secret.

Emergency rotation:

1. Rotate `webhook_secret` to a new value.
2. Pause the upload source (admin → source bulk pause).
3. Issue new signed tokens; replay the previous batch
   manually if any uploads were in-flight.

Note: the connector validates incoming uploads against a
per-source secret captured at Connect time, so the platform
can rotate the secret without restarting the API.

## 3. Quota / rate-limit incidents

The receiver does not poll an upstream API, so there is no
external rate-limit. The platform's API gateway enforces
per-tenant ingress quotas via the standard
`CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` knob.

- A burst of failed signature validations surfaces as
  `400 Bad Request` from `/v1/webhooks/upload_portal`.
- The audit log emits a `webhook.signature_invalid` event
  for each rejection; alert if the rate exceeds 10/min.

## 4. Outage detection & recovery

The receiver is in-process; outages match the API binary's
health signals.

Common outage signatures:

- 5xx burst from `/v1/webhooks/upload_portal` correlated
  with the API binary's restart loop.
- Multipart parse failures spiking after a TLS rotation
  truncates request bodies.

Recovery:

1. Inspect the API binary's logs for upload-parse errors.
2. Restart the API binary if the request body is being
   buffered improperly (e.g. nginx `client_max_body_size`
   misconfig).
3. Clients should retry uploads — the receiver has no
   server-side queue, so missed uploads must be re-POSTed.

## 5. Common error codes

| Status | Body | Meaning | Action |
|--------|------|---------|--------|
| `400` | `webhook signature mismatch` | Bad HMAC / wrong secret | §2 |
| `400` | `mime "<type>" not in allowed list` | Client posted disallowed MIME | Reissue signed token, or update `allowed_mime` |
| `400` | `upload exceeds max_size_bytes=<n>` | Body bigger than limit | Lift limit or instruct client to chunk |
| `413` | n/a | API gateway-level body limit | Raise `client_max_body_size` |
| `429` | n/a | Per-tenant rate-limit | §3 |
