# Local-first retrieval protocol

## Goal

For every retrieval query a client makes, the on-device shard is
the **default substrate**. The remote `POST /v1/retrieve` is a
controlled fallback that the client invokes only when:

1. The server explicitly opts the client into remote retrieval
   (`prefer_local: false` in the device-first hint), or
2. The local coverage is insufficient for the query (the
   client's own coverage check), or
3. The privacy mode demands a remote-only answer.

Phase 6 / Task 12 (this doc), Phase 6 / Task 15 (server hint),
Phase 5 / Task 12 (server-side coverage endpoint).

## Decision tree

```
                     ┌─────────────────────────────────────────────┐
client warm boot ─►  │ POST /v1/retrieve                           │
(or query)           │   request includes device_tier + privacy    │
                     └─────────────┬───────────────────────────────┘
                                   │
                       ┌───────────▼────────────┐
                       │ server: device_first   │
                       │ policy.Decide()         │
                       └───────────┬────────────┘
                                   │
                  prefer_local=false│        prefer_local=true
                                   │                │
                       ┌───────────▼─┐    ┌─────────▼──────────────┐
                       │ remote-only │    │ client: read local     │
                       │ response.   │    │ shard at version v     │
                       │ done.       │    │ from /v1/shards/...    │
                       └─────────────┘    └─────────┬──────────────┘
                                                    │
                                          ┌─────────▼──────────────┐
                                          │ client: GET            │
                                          │ /v1/shards/:t/coverage │
                                          │   ?privacy_mode=...    │
                                          └─────────┬──────────────┘
                                                    │
                                       coverage_ratio │ coverage_ratio
                                       ≥ threshold    │ < threshold or
                                                      │ is_authoritative=false
                                          ┌─────────▼─┐    ┌────────▼──────────┐
                                          │ serve     │    │ fall back to      │
                                          │ from local│    │ POST /v1/retrieve │
                                          │ shard.    │    │ (remote).         │
                                          └───────────┘    └───────────────────┘
```

## Server side

### `POST /v1/retrieve` device-first hint

The Go-side decision tree lives in
<ref:internal/policy/device_first.go>. It returns `prefer_local`
and `local_shard_version` based on:

| Input | Source | Effect |
|-------|--------|--------|
| `device_tier` (low/mid/high/unknown) | request body | low/unknown → `prefer_local=false` |
| channel allow-local flag | channel policy (default true) | false → `prefer_local=false` |
| privacy mode | request → channel policy (effective mode wins) | `no-ai` → `prefer_local=false` |
| local shard version | `shard.Repository.LatestVersion` | `0` → `prefer_local=false` |

Each rejection is surfaced as a stable `prefer_local_reason`
label so audit logs can trace why a query was routed remote.

### `GET /v1/shards/:tenant_id/coverage`

Returns coverage metadata for the supplied scope. Required query
param: `privacy_mode`. Optional: `user_id`, `channel_id`. Response:

```json
{
  "tenant_id": "tenant-123",
  "privacy_mode": "hybrid",
  "shard_version": 42,
  "shard_chunks": 14820,
  "corpus_chunks": 21034,
  "coverage_ratio": 0.7045,
  "is_authoritative": true
}
```

* `is_authoritative` is `true` when the server has loaded a
  ground-truth corpus chunk count from the `CoverageRepo`. When
  the repo can't supply the count (e.g. the implementation only
  returns shard size), the field is `false` and clients MUST
  treat the ratio as advisory.
* `reason` is populated when no shard exists yet for the scope
  (`"no_shard"`).

The Go-side response shape is `shard.CoverageResponse`
(<ref:internal/shard/handler.go>).

## Client side

### Coverage threshold

Default threshold: **0.50**. A shard with at least 50% of the
tenant's chunks is considered "authoritative enough" to serve
the average top-k=10 query. Higher thresholds (e.g. 0.75) reduce
false-confidence local hits at the cost of more remote fallbacks.

### Cache

The coverage response is safe to cache for 60s. The shard
manifest carries its own version, so the client can detect a
shard refresh (manifest version increments) without re-fetching
coverage.

### Failure modes

| Failure | Client behaviour |
|---------|------------------|
| coverage endpoint returns 5xx | Treat as non-authoritative, fall back to remote |
| local shard apply fails | Re-`syncShard`, then re-query |
| remote retrieval fails | Surface error to caller; do NOT silently serve stale local |

## Privacy-mode interaction

| Privacy mode | `prefer_local` allowed | Notes |
|--------------|------------------------|-------|
| `no-ai` | no | All retrieval blocked |
| `local-only` | yes | Server returns no remote hits at all |
| `local-api` | yes | Local first; remote fallback to local-api plane |
| `hybrid` | yes | Local first; remote fallback unrestricted |
| `remote` | yes | Local first as an optimisation; remote fallback unrestricted |

## Auditability

When `prefer_local=true` and the client serves locally, the
client MUST emit a `local_retrieval` audit event with the
shard_version and coverage_ratio it observed. When the client
falls back to remote despite a local hint, it MUST emit a
`local_fallback` event with the reason. Both events are batched
and sent to `POST /v1/audit/events` on the next foreground sync.
