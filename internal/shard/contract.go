package shard

import (
	"context"
	"errors"
)

// ShardClientContract is the Go-side mirror of the on-device shard
// client surface. The mobile (iOS / Android UniFFI) and desktop
// (Electron N-API) tiers each implement this contract in their
// platform-specific binding so the server can reason about what an
// on-device client *must* support without knowing the binding
// language.
//
// The contract is deliberately small. Anything more than the four
// methods below — query parsing, ranking, model loading — belongs
// inside the Rust knowledge core (`kennguy3n/knowledge`) and is
// invisible to the Go binary.
//
// Method semantics:
//
//   - SyncShard fetches the freshest manifest for the supplied
//     scope (tenant + optional user/channel + privacy mode),
//     consuming the server's `GET /v1/shards/:tenant_id` response.
//     Implementations must persist the returned manifest atomically
//     (no half-applied state across crashes) and reject manifests
//     whose tenant_id does not match the device's owning tenant.
//
//   - ApplyDelta applies the add/remove operations from
//     `GET /v1/shards/:tenant_id/delta?since=<v>` against the local
//     index. Implementations must apply add operations before
//     remove operations so a chunk that was renamed (added under a
//     new id, removed under the old) does not end up missing
//     between the two writes.
//
//   - LocalRetrieve runs a query against the on-device shard and
//     returns ranked matches. Implementations must enforce the
//     same `policy.PolicySnapshot` that gated the shard's
//     generation; if the shard's snapshot is missing or the
//     resolver reports a stricter mode at query time, the
//     implementation must return an empty result and surface a
//     `prefer_remote` hint to the caller (see
//     `docs/contracts/local-first-retrieval.md`).
//
//   - CryptographicForget destroys every artifact derived from the
//     supplied tenant_id (shard manifests, chunk indices, KEKs/DEKs
//     stored in the platform secure-enclave, on-disk caches) and
//     returns only after every tier has been swept. The
//     implementation MUST be idempotent so a partial failure
//     followed by a retry converges to the same end state.
//
// Tenant isolation: every method takes an explicit tenantID. The
// implementation must reject any call whose tenantID does not match
// the device's locally-attested owning tenant so a hostile caller
// can't drive a sync against another tenant's keys.
type ShardClientContract interface {
	// SyncShard retrieves the freshest shard manifest matching
	// the supplied scope. Returns the manifest as the server
	// emitted it; the caller is responsible for any local cache.
	SyncShard(ctx context.Context, tenantID string, scope ShardScope) (*ShardManifest, error)

	// ApplyDelta applies the supplied delta operations against
	// the on-device shard. Adds are applied before removes; the
	// final state matches the server's at version delta.To.
	ApplyDelta(ctx context.Context, tenantID string, delta ShardDelta) error

	// LocalRetrieve runs query against the on-device shard. The
	// returned slice is ranked best-first and contains at most
	// topK entries. An empty slice plus a nil error means the
	// shard is healthy but matched no chunks.
	LocalRetrieve(ctx context.Context, tenantID string, query LocalQuery) (LocalRetrievalResult, error)

	// CryptographicForget destroys every on-device artifact for
	// tenantID. Idempotent — safe to retry on failure.
	CryptographicForget(ctx context.Context, tenantID string) error
}

// ShardScope narrows a SyncShard call to a (user, channel, privacy
// mode) tuple within a tenant. Empty UserID / ChannelID match the
// tenant-wide shards.
type ShardScope struct {
	UserID      string
	ChannelID   string
	PrivacyMode string
}

// Validate returns an error when required fields are missing.
// PrivacyMode is required: an unset mode would force the resolver
// to fall back to a default, which is exactly the foot-gun the
// scope is meant to prevent.
func (s ShardScope) Validate() error {
	if s.PrivacyMode == "" {
		return errors.New("shard: ShardScope.PrivacyMode required")
	}
	return nil
}

// ShardDelta is the wire shape of an apply-delta call. Mirrors the
// JSON body of `GET /v1/shards/:tenant_id/delta?since=<v>` so the
// platform binding can decode the response straight into this
// struct.
type ShardDelta struct {
	From       int64          `json:"from_version"`
	To         int64          `json:"to_version"`
	Operations []DeltaOp      `json:"operations"`
	IsFullSync bool           `json:"is_full_sync"`
	Manifest   *ShardManifest `json:"manifest,omitempty"`
}

// LocalQuery is a single-shot retrieval against the on-device
// shard. Mirrors the request shape of POST /v1/retrieve so the
// same query JSON travels both paths — see
// `docs/contracts/local-first-retrieval.md` for the decision tree.
type LocalQuery struct {
	Query       string   `json:"query"`
	TopK        int      `json:"top_k"`
	Channels    []string `json:"channels,omitempty"`
	PrivacyMode string   `json:"privacy_mode,omitempty"`
	SkillID     string   `json:"skill_id,omitempty"`
}

// LocalRetrievalResult is the on-device counterpart of
// retrieval.RetrieveResponse. The two shapes intentionally diverge:
// the on-device tier never observes server-side observability
// (TraceID, RetrieveTimings) and instead reports CoverageRatio so
// the caller can decide whether to fall back to the remote API.
type LocalRetrievalResult struct {
	Hits          []LocalHit `json:"hits"`
	ShardVersion  int64      `json:"shard_version"`
	CoverageRatio float64    `json:"coverage_ratio"`
	PreferRemote  bool       `json:"prefer_remote"`
	Reason        string     `json:"reason,omitempty"`
}

// LocalHit is the slimmed-down on-device match. Excludes server-
// side metadata (trace_id) and policy-rule references because the
// on-device tier does not have the policy resolver wired in — the
// shard is already policy-bounded by construction.
type LocalHit struct {
	ID           string  `json:"id"`
	Score        float32 `json:"score"`
	DocumentID   string  `json:"document_id,omitempty"`
	BlockID      string  `json:"block_id,omitempty"`
	Title        string  `json:"title,omitempty"`
	URI          string  `json:"uri,omitempty"`
	PrivacyLabel string  `json:"privacy_label,omitempty"`
	Text         string  `json:"text,omitempty"`
}
