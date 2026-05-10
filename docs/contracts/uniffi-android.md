# UniFFI / Android AAR contract

## Surface

The Android knowledge core ships as an AAR produced by
[`uniffi-rs`][uniffi] from the `kennguy3n/knowledge` crate. The
Kotlin import is named `com.kennguy3n.knowledge`. Android apps
consume it via:

```kotlin
import com.kennguy3n.knowledge.KnowledgeCore

val core = KnowledgeCore.open(homeDir = filesDir, tenantId = tenantId)
```

The on-device contract mirrors the Go-side
`shard.ShardClientContract` interface
(<ref:internal/shard/contract.go>). The Kotlin signatures use
suspend functions (mapped from the Rust async surface) and Kotlin
data classes for the wire types.

## Methods

```kotlin
suspend fun syncShard(tenantId: String, scope: ShardScope): ShardManifest
suspend fun applyDelta(tenantId: String, delta: ShardDelta)
suspend fun localRetrieve(tenantId: String, query: LocalQuery): LocalRetrievalResult
suspend fun cryptographicForget(tenantId: String)
```

## Types

```kotlin
data class ShardScope(val userId: String, val channelId: String, val privacyMode: String)
data class ShardDelta(val fromVersion: Long, val toVersion: Long, val operations: List<ShardOp>)
data class LocalQuery(val query: String, val topK: UInt, val channels: List<String>, val privacyMode: String, val skillId: String?)
data class LocalRetrievalResult(val hits: List<LocalHit>, val shardVersion: Long, val coverageRatio: Double, val preferRemote: Boolean, val reason: String?)
data class LocalHit(val id: String, val score: Double, val title: String, val uri: String, val privacyLabel: String, val text: String?)
```

JSON wire format MUST match `internal/shard/contract.go` field
tags exactly.

## Packaging

* The AAR ships ABI-split native libraries:
  `arm64-v8a`, `armeabi-v7a`, `x86_64`.
* The Rust crate is built with `--features = ["bonsai", "android"]`;
  the `bonsai` feature pulls in the on-device SLM bridge.
* The AAR is signed with the platform's release certificate and
  distributed through the Maven repository under
  `com.kennguy3n:knowledge-core`.

## JNI bridge

The Kotlin layer wraps a JNI handle returned by `KnowledgeCore.open`.
Lifetime rules:

* The handle is `AutoCloseable`. Callers MUST `close()` it when
  the user signs out so the per-tenant SQLite cipher key is wiped.
* The handle is bound to the calling thread's `CoroutineDispatcher`;
  crossing dispatchers requires re-`open`ing the handle (the JNI
  reference is not thread-safe).
* Forgetting to `close()` leaks a 100kB native allocation and
  prevents `cryptographicForget` from completing.

## Background sync

Android uses `WorkManager` periodic tasks to poll for shard
deltas. See [`background-sync.md`](background-sync.md) for the
schedule the client receives from `GET /v1/sync/schedule`.

## Cryptographic forget

`cryptographicForget` MUST:

1. Delete the per-tenant SQLite shard file under
   `filesDir/<tenant_id>/`.
2. Erase the per-tenant key from the Android Keystore.
3. Cancel any outstanding `WorkRequest` that targets the tenant.
4. Emit an `audit_event` of kind `cryptographic_forget` to the
   server's `POST /v1/audit/events` endpoint when the device is
   next online.

[uniffi]: https://mozilla.github.io/uniffi-rs/
