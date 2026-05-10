# Background sync protocol

## Goal

Each on-device client keeps its shard fresh by polling the
delta endpoint on a platform-native scheduler. The polling
schedule is **server-driven**: clients call
`GET /v1/sync/schedule` on warm-boot to learn the recommended
intervals so the server can re-tune (e.g. shorten foreground
sync during a heavy reindex) without an app release.

## `GET /v1/sync/schedule`

Tenant-scoped (auth-required). Response:

```json
{
  "tenant_id": "tenant-123",
  "foreground_sync_interval_ns": 60000000000,
  "background_sync_interval_ns": 900000000000,
  "min_foreground_sync_seconds": 30,
  "min_background_sync_seconds": 300,
  "jitter_seconds": 30
}
```

| Field | Meaning |
|-------|---------|
| `foreground_sync_interval_ns` | recommended interval while the app is open |
| `background_sync_interval_ns` | recommended interval while the app is backgrounded |
| `min_foreground_sync_seconds` | hard floor — clients MUST NOT poll faster |
| `min_background_sync_seconds` | hard floor — clients MUST NOT poll faster |
| `jitter_seconds` | uniform-random jitter applied per poll |

The Go-side response shape is `b2c.SyncSchedule`
(<ref:internal/b2c/handler.go>). Defaults: foreground 60 s,
background 15 min, jitter ±30 s.

## Platform-specific scheduling

### iOS — `BGAppRefreshTask`

```swift
import BackgroundTasks

let req = BGAppRefreshTaskRequest(identifier: "com.kennguy3n.knowledge.shardSync")
req.earliestBeginDate = Date(timeIntervalSinceNow: schedule.backgroundSyncInterval)
try BGTaskScheduler.shared.submit(req)
```

The `BGAppRefreshTaskRequest` handler MUST:

1. Call `GET /v1/shards/:tenant_id/delta?since=<v>` for each
   active scope. `<v>` is the runtime's stored shard version.
2. Apply each delta via `KnowledgeCore.applyDelta`.
3. Reschedule itself with the latest `BackgroundSyncInterval`.
4. Complete within the iOS-imposed 30 s budget; if the delta
   apply takes longer, abort and rely on the next refresh.

iOS does **not** guarantee the request fires on the supplied
interval — the scheduler may delay or skip it on a tight
battery / thermal budget. The runtime treats every delta apply
as best-effort; coverage drift is bounded by the staleness check
in `GET /v1/shards/:tenant_id/coverage`.

### Android — `WorkManager`

```kotlin
val req = PeriodicWorkRequestBuilder<ShardSyncWorker>(
    schedule.backgroundSyncInterval.toJavaDuration()
).addTag("shard-sync")
 .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
 .build()

WorkManager.getInstance(ctx).enqueueUniquePeriodicWork(
    "shard-sync",
    ExistingPeriodicWorkPolicy.KEEP,
    req,
)
```

The `ShardSyncWorker.doWork` MUST:

1. Call `GET /v1/shards/:tenant_id/delta?since=<v>`.
2. Apply each delta via `KnowledgeCore.applyDelta`.
3. Return `Result.success()` on a clean run; on a partial
   failure return `Result.retry()` so WorkManager backs off
   exponentially.

WorkManager's minimum periodic interval is 15 min; if the
server returns a smaller `background_sync_interval_ns`, the
client MUST clamp to 15 min.

### Desktop — system timer / `powerMonitor`

```ts
import { powerMonitor } from "electron";

powerMonitor.on("idle", async () => {
  // best-effort sync while the user isn't typing
  await syncAllScopes();
});

setInterval(syncAllScopes, schedule.backgroundSyncInterval);
```

The desktop runtime uses BOTH:

* a `setInterval` timer at `background_sync_interval_ns` for the
  steady-state sync, AND
* an `idle` listener that fires opportunistically whenever the
  user is away from the keyboard for ≥ 5 minutes.

The idle path is purely an optimisation; if `powerMonitor` is
unavailable (e.g. headless CI), only the timer runs.

## Failure handling

| Failure | Client behaviour |
|---------|------------------|
| `GET /v1/sync/schedule` returns 5xx | use `DefaultSyncSchedule` constants from `internal/b2c/handler.go` |
| `GET /v1/shards/:tenant_id/delta` returns 5xx | exponential backoff up to 5 attempts, then drop to next scheduled poll |
| `applyDelta` throws version mismatch | force a full `syncShard` and replace the local shard |
| Network unavailable | the platform scheduler skips the run; do not wake the radio |

## Rate-limit guard

The client MUST honour the `min_*_sync_seconds` fields: even if
a config glitch sets `foreground_sync_interval_ns` to 1 s, the
client must clamp to 30 s. This prevents a rogue release from
DoS-ing the API.

The server-side rate limiter (Redis token bucket per tenant)
catches anything that slips through; clients receiving a 429
MUST back off for at least 60 s before retrying.

## Audit trail

Every sync run that successfully applies a delta MUST emit a
`shard_sync` audit event with `from_version`, `to_version`,
`bytes_applied`, and `duration_ms`. The events batch up and ship
on the next foreground sync.
