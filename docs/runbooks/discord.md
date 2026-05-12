# Discord runbook (`discord`)

> Source: [`internal/connector/discord/discord.go`](../../internal/connector/discord/discord.go)
> · API base: `https://discord.com/api/v10`
> · Webhook: not used (Discord uses Gateway WebSocket; out of scope for poll connector)
> · Delta cursor: `after=<message_id>` snowflake on `/channels/<id>/messages`

## 1. Auth model

Discord bot token issued to a bot user added to one or more
guilds.

```json
{ "bot_token": "MTAxMjM0..." }
```

Header: `Authorization: Bot MTAxMjM0...` (note the literal `Bot`
prefix).

Required Discord permissions on each ingestable channel:

- `View Channel`
- `Read Message History`

## 2. Credential rotation

Routine rotation:

1. Discord developer portal → **Applications** → your app →
   **Bot** → **Reset Token**.
2. PATCH the source credential via the admin portal.
3. Verify: `GET /users/@me` returns the bot identity.
4. Confirm guilds list via `GET /users/@me/guilds`.

Emergency rotation:

1. Discord dev portal → **Bot** → **Reset Token** (revokes
   immediately).
2. If the leaked token had elevated permissions, also rotate the
   bot's OAuth client secret in **OAuth2** → **Reset Secret**.
3. Audit guild audit logs (Discord → server settings → audit
   log) for any actions taken by the bot during the leak window.

## 3. Quota / rate-limit incidents

Discord enforces per-route + global rate limits. The connector
hits routes:

- `GET /users/@me` (cheap)
- `GET /users/@me/guilds` (5 req / 5s global)
- `GET /guilds/<id>/channels` (per-guild bucket)
- `GET /channels/<id>/messages` (per-channel bucket, 50 / sec)

Throttle signal:

- `429 Too Many Requests` with `Retry-After` AND a
  `X-RateLimit-Bucket` header.

Wraps to [`connector.ErrRateLimited`](../../internal/connector/source_connector.go).

Knobs:

- `CONTEXT_ENGINE_RATELIMIT_<source_id>_RPS` defaults to 25
  (matches per-channel 50/s halved); raise for high-volume
  channels.
- Discord's `X-RateLimit-Reset-After` header is honoured by the
  adaptive limiter.

## 4. Outage detection & recovery

Discord publishes status at https://discordstatus.com.

Common outage signatures:

- `500/503` from `discord.com/api/v10/*`.
- `502 Bad Gateway` during Discord deploys.

Recovery:

1. Confirm via discordstatus.com.
2. The iterator resumes from the last successful message
   snowflake (`after=<id>`).
3. For very long outages (>24h), validate that the bot is still
   in the guild — Discord may auto-remove bots that fail health
   checks.

## 5. Common error codes

| Upstream | Body | Meaning | Action |
|----------|------|---------|--------|
| `401` | `{"message":"401: Unauthorized","code":0}` | Bot token revoked | §2 routine rotation |
| `403` | `{"message":"Missing Access","code":50001}` | Bot lost channel permission | Re-grant View Channel + Read Message History |
| `404` | `{"message":"Unknown Channel","code":10003}` | Channel deleted | Remove from namespace filter |
| `429` | `{"message":"You are being rate limited.","retry_after":1.5}` | Per-bucket throttle | §3; respect `retry_after` |
| `502` | n/a | Discord deploy in progress | §4; iterator will retry |
