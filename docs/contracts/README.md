# Client-side contracts

Each file in this directory is the source of truth for a contract
between the server (this repo) and a client surface (mobile,
desktop, B2C web). The matching Go-side types live in
`internal/shard`, `internal/policy`, `internal/b2c`, and
`internal/models`; if you change one, change the other.

| Document | Surface | Server-side anchor |
|----------|---------|--------------------|
| [`uniffi-ios.md`](uniffi-ios.md) | iOS XCFramework | `internal/shard/contract.go` |
| [`uniffi-android.md`](uniffi-android.md) | Android AAR | `internal/shard/contract.go` |
| [`napi-desktop.md`](napi-desktop.md) | Electron N-API | `internal/shard/contract.go` |
| [`local-first-retrieval.md`](local-first-retrieval.md) | All on-device clients | `GET /v1/shards/:tenant_id/coverage` + `internal/policy/device_first.go` |
| [`bonsai-integration.md`](bonsai-integration.md) | All on-device clients | `internal/models/` + `GET /v1/models/catalog` |
| [`b2c-retrieval-sdk.md`](b2c-retrieval-sdk.md) | B2C clients | `internal/b2c/handler.go` |
| [`privacy-strip-render.md`](privacy-strip-render.md) | All clients | `internal/retrieval.PrivacyStrip` |
| [`background-sync.md`](background-sync.md) | All on-device clients | `GET /v1/sync/schedule` |

Phase context: contracts cover Phase 5 (on-device shard sync),
Phase 6 (B2C client SDK), and Phase 8 (Bonsai-1.7B integration +
shard eviction).
