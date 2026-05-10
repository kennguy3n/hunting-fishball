# N-API / desktop (Electron) contract

## Surface

The desktop knowledge core ships as a Node N-API addon produced
from the `kennguy3n/knowledge` crate via [`napi-rs`][napi-rs]. The
JavaScript import is named `@kennguy3n/knowledge-core`. Desktop
apps (Electron + Skytrack desktop) consume it via:

```ts
import { KnowledgeCore } from "@kennguy3n/knowledge-core";

const core = await KnowledgeCore.open({ homeDir: app.getPath("userData"), tenantId });
```

The on-device contract mirrors the Go-side
`shard.ShardClientContract` interface
(<ref:internal/shard/contract.go>).

## Methods

```ts
class KnowledgeCore {
  syncShard(tenantId: string, scope: ShardScope): Promise<ShardManifest>;
  applyDelta(tenantId: string, delta: ShardDelta): Promise<void>;
  localRetrieve(tenantId: string, query: LocalQuery): Promise<LocalRetrievalResult>;
  cryptographicForget(tenantId: string): Promise<void>;
  close(): Promise<void>;
}
```

## Types

```ts
interface ShardScope { userId: string; channelId: string; privacyMode: string; }
interface ShardDelta { fromVersion: bigint; toVersion: bigint; operations: ShardOp[]; }
interface LocalQuery {
  query: string;
  topK: number;
  channels?: string[];
  privacyMode?: string;
  skillId?: string;
}
interface LocalRetrievalResult {
  hits: LocalHit[];
  shardVersion: bigint;
  coverageRatio: number;
  preferRemote: boolean;
  reason?: string;
}
interface LocalHit {
  id: string;
  score: number;
  title: string;
  uri: string;
  privacyLabel: string;
  text?: string;
}
```

JSON wire format MUST match `internal/shard/contract.go` field
tags exactly. `int64` Rust values are mapped to JS `bigint` to
avoid silent truncation past 2^53.

## Packaging

* The addon ships pre-built per-platform binaries:
  `darwin-arm64`, `darwin-x64`, `linux-x64`, `linux-arm64`,
  `win32-x64`.
* The Rust crate is built with `--features = ["bonsai", "napi"]`;
  the `bonsai` feature pulls in the on-device SLM bridge.
* The npm package is published from `kennguy3n/knowledge`'s CI.

## Process model

The addon runs in the Electron *main* process; the renderer
talks to it via IPC. Reasons:

* The on-device SLM (Bonsai-1.7B GGUF) keeps a 1.0–3.4 GB working
  set; we don't want it duplicated per-renderer.
* `cryptographicForget` must be able to atomically erase the
  per-tenant SQLite cipher key and shard file — only the main
  process holds the file handles.
* The Bonsai inference is CPU-heavy; the renderer would block
  the UI without a worker indirection.

## Background sync

Desktop uses Electron's
`powerMonitor.on('idle', ...)` (or a system timer on
non-`powerMonitor` platforms) to poll for shard deltas. See
[`background-sync.md`](background-sync.md) for the schedule the
client receives from `GET /v1/sync/schedule`.

## Cryptographic forget

`cryptographicForget` MUST:

1. Delete the per-tenant SQLite shard file under
   `app.getPath("userData")/<tenant_id>/`.
2. Erase the per-tenant key from the OS keychain
   (Keychain on macOS, Credential Vault on Windows, libsecret on
   Linux).
3. Cancel any outstanding sync timer that targets the tenant.
4. Emit an `audit_event` of kind `cryptographic_forget` to the
   server's `POST /v1/audit/events` endpoint when the device is
   next online.

[napi-rs]: https://napi.rs/
