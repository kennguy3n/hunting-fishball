# B2C retrieval SDK contract

## Overview

The B2C surface (Skytrack mobile, b2c-kchat, kchat-portal) calls
the same `POST /v1/retrieve` as the B2B surface, with two extra
boot-time endpoints to bridge the platform's slower release cycle:

* `GET /v1/health` — liveness ping the splash screen calls before
  attempting a retrieval. Cheap (no DB).
* `GET /v1/capabilities` — feature surface so a B2C UI built
  three months ago can adapt to a server that has flipped a
  backend on/off without forcing an app update.
* `GET /v1/sync/schedule` — recommended sync intervals; see
  [`background-sync.md`](background-sync.md).

All three endpoints live in <ref:internal/b2c/handler.go>.

## `GET /v1/health`

Tenant-scoped (auth-required) liveness probe.

Response (200):

```json
{ "status": "ok", "time": "2026-05-10T03:14:15.926Z", "version": "v0.1.0" }
```

Clients SHOULD call this on cold start before showing the
retrieval UI. The 200 is unconditional when the API process is
up; the dependency-aware probe is `/readyz` (used by k8s, not
the B2C client).

## `GET /v1/capabilities`

Tenant-scoped (auth-required). Reports which retrieval backends
are wired and which Phase 4 / 6 features are live.

Response (200):

```json
{
  "enabled_backends": ["vector", "bm25", "graph", "memory"],
  "privacy_modes": ["no-ai", "local-only", "local-api", "hybrid", "remote"],
  "local_shard_sync": true,
  "privacy_strip": true,
  "device_first": true,
  "server_version": "v0.1.0"
}
```

| Field | Meaning |
|-------|---------|
| `enabled_backends` | non-nil retrieval backends in the server's wiring |
| `privacy_modes` | full ladder; the channel policy decides which modes the user sees |
| `local_shard_sync` | shard endpoints are mounted (Phase 5) |
| `privacy_strip` | every retrieval row carries the structured `privacy_strip` field (Phase 4 invariant) |
| `device_first` | server emits `prefer_local` in `RetrieveResponse` (Phase 6 / Task 15) |

Client guidance:

* Cache for 5 minutes to absorb the typical retrieval session.
* If `device_first == false`, do NOT include `device_tier` in the
  retrieve request; the server will ignore it.
* If a backend the client expects (e.g. `graph`) is missing,
  surface a "limited mode" indicator rather than retrying.

## `POST /v1/retrieve` (B2C usage)

The B2C wire format is identical to the B2B contract. The B2C
surface SHOULD always set:

* `device_tier`: one of `low` / `mid` / `high` so the device-first
  hint can fire.
* `privacy_mode`: the channel's declared mode so the response is
  policy-bounded.

Response fields the B2C surface MUST inspect:

| Field | Why |
|-------|-----|
| `prefer_local` | route the next retrieval through the on-device shard |
| `local_shard_version` | learn whether a shard exists for this scope |
| `policy.applied` | drive the "results from N backends" UI badge |
| `policy.degraded` | drive the "limited mode" UI badge |
| `policy.privacy_mode` | drive the privacy strip header |
| `hits[*].privacy_strip` | drive the per-row privacy disclosure (see [`privacy-strip-render.md`](privacy-strip-render.md)) |

## Versioning

Both `/v1/health` and `/v1/capabilities` are versioned `v1` and
will not break compatibility within the v1 release line. New
fields will be added with `omitempty` so older clients can
ignore them.

## Auth

All B2C SDK endpoints sit under the same authPlaceholder
middleware as the rest of `/v1/*`: the request MUST present an
`X-Tenant-ID` header (and, in production, the bearer token the
auth tier issues). A missing tenant ID returns 401.
