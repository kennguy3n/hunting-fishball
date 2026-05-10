# UniFFI / iOS XCFramework contract

## Surface

The iOS knowledge core ships as an XCFramework produced by
[`uniffi-rs`][uniffi] from the `kennguy3n/knowledge` crate. The
Swift import is named `KnowledgeCore`. iOS apps consume it via:

```swift
import KnowledgeCore
let core = try KnowledgeCore.open(homeDir: appSupport, tenantID: tenantID)
```

The Rust trait the framework exposes is the on-device mirror of
the Go-side `shard.ShardClientContract` interface
(<ref:internal/shard/contract.go>). The two MUST stay in lockstep.
Any signature change here requires a matching change in
`internal/shard/contract.go` and a regenerated UniFFI bridge.

## Methods

```swift
// Pull (or refresh) the shard manifest for the supplied scope.
// The server endpoint is `GET /v1/shards/:tenant_id`; UniFFI
// projects that into a strongly-typed `ShardManifest`.
func syncShard(tenantId: String, scope: ShardScope) async throws -> ShardManifest

// Apply a delta returned by `GET /v1/shards/:tenant_id/delta`.
// The runtime is responsible for monotonic version tracking;
// applying a delta whose `from_version` does not equal the
// runtime's stored version MUST throw.
func applyDelta(tenantId: String, delta: ShardDelta) async throws

// Run the local retrieval pipeline against the on-device shard.
// `LocalRetrievalResult.preferRemote` mirrors the server-side
// `prefer_local` hint (inverted) — when the local coverage
// cannot satisfy the query, the runtime sets it to true so the
// caller falls back to `POST /v1/retrieve`.
func localRetrieve(tenantId: String, query: LocalQuery) async throws -> LocalRetrievalResult

// Tear down all on-device material for a tenant. Idempotent:
// calling it on a non-existent tenant returns successfully.
func cryptographicForget(tenantId: String) async throws
```

## Types

```swift
struct ShardScope { let userId: String; let channelId: String; let privacyMode: String }
struct ShardDelta { let fromVersion: Int64; let toVersion: Int64; let operations: [ShardOp] }
struct LocalQuery { let query: String; let topK: UInt32; let channels: [String]; let privacyMode: String; let skillId: String? }
struct LocalRetrievalResult { let hits: [LocalHit]; let shardVersion: Int64; let coverageRatio: Double; let preferRemote: Bool; let reason: String? }
struct LocalHit { let id: String; let score: Double; let title: String; let uri: String; let privacyLabel: String; let text: String? }
```

JSON wire format MUST match `internal/shard/contract.go` field
tags exactly so the same struct can be marshalled by either side.

## Packaging

* The framework is built fat-binary with simulator + device slices.
* The Rust crate is built with `--features = ["bonsai", "ios"]`;
  the `bonsai` feature pulls in the on-device SLM bridge.
* The XCFramework is signed with the team's distribution
  certificate and shipped through the platform's release pipeline
  in `kennguy3n/knowledge`.

## Background sync

iOS uses `BGAppRefreshTask` to poll for shard deltas. See
[`background-sync.md`](background-sync.md) for the schedule the
client receives from `GET /v1/sync/schedule`.

## Cryptographic forget

`cryptographicForget` MUST:

1. Delete the per-tenant SQLite shard file under the app's
   `Application Support/<tenant_id>` directory.
2. Erase the per-tenant key from the iOS Keychain (the encryption
   key for the SQLite cipher).
3. Cancel any outstanding `BGAppRefreshTask` that targets the
   tenant.
4. Emit an `audit_event` of kind `cryptographic_forget` to the
   server's `POST /v1/audit/events` endpoint when the device is
   next online.

[uniffi]: https://mozilla.github.io/uniffi-rs/
