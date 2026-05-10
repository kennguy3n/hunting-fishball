// Package shard implements the Phase 5 server-side shard sync API.
//
// A shard is the unit the on-device knowledge core syncs from the
// server: a serialised list of chunk IDs (and, optionally, the
// embeddings) that satisfies the sender's policy at the time the
// shard was generated. The (tenant, user, channel, privacy_mode)
// scope keys the shard, and a monotonically-increasing
// `shard_version` lets clients ask "what changed since I last
// synced version N?".
//
// The package contains:
//
//   - ShardManifest / ShardChunk GORM models and the shards /
//     shard_chunks tables (migrations/006_shards.sql).
//   - Repository, the CRUD seam used by the HTTP handler and the
//     generator worker.
//   - Generator, the worker that walks the storage plane (Postgres
//     chunk metadata + Qdrant embeddings) under a PolicySnapshot
//     and persists a new shard version.
//   - Delta computation (delta.go) that compares two versions'
//     chunk-ID sets and emits add/remove operations.
//   - Forget (forget.go), the orchestrator that sweeps every storage
//     tier when a tenant's keys are destroyed.
//
// Tenant isolation matches the rest of the codebase: every read MUST
// lead with `tenant_id = ?` and the repository refuses to run a
// query without one.
package shard

import (
	"errors"
	"time"
)

// ShardStatus is the lifecycle marker on a shards row.
type ShardStatus string

const (
	// ShardStatusPending is the initial state — manifest row is
	// allocated but the chunk-ID set has not yet been persisted.
	ShardStatusPending ShardStatus = "pending"

	// ShardStatusReady means the generator finished and the
	// chunk IDs are in shard_chunks.
	ShardStatusReady ShardStatus = "ready"

	// ShardStatusFailed means the generator gave up after retries.
	ShardStatusFailed ShardStatus = "failed"

	// ShardStatusSuperseded means a newer shard version replaced
	// this one. Older versions are kept around so delta sync can
	// answer "what changed between old and new".
	ShardStatusSuperseded ShardStatus = "superseded"
)

// LifecycleState is the marker on a tenant_lifecycle row used by
// the cryptographic forgetting flow.
type LifecycleState string

const (
	// LifecyclePendingDeletion marks a tenant whose key destruction
	// has been requested. The pipeline should stop accepting new
	// writes for this tenant; the forget orchestrator drains and
	// then sweeps every storage tier.
	LifecyclePendingDeletion LifecycleState = "pending_deletion"

	// LifecycleDeleted is terminal — every storage tier has been
	// swept and the DEKs destroyed. The row is preserved for audit.
	LifecycleDeleted LifecycleState = "deleted"
)

// ShardManifest is the GORM model for the shards table. Schema lives
// in migrations/006_shards.sql.
type ShardManifest struct {
	// ID is a 26-char ULID. Unique per (tenant, user, channel,
	// privacy_mode, shard_version) tuple.
	ID string `gorm:"type:char(26);primaryKey;column:id" json:"id"`

	// TenantID is the multi-tenant scope. Every read query MUST
	// include `tenant_id = ?`.
	TenantID string `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`

	// UserID narrows the shard to a specific user. Empty string
	// means "tenant-wide" — the shard is shared across users in the
	// tenant.
	UserID string `gorm:"type:varchar(64);column:user_id" json:"user_id,omitempty"`

	// ChannelID narrows the shard to a channel. Empty string means
	// "tenant-wide".
	ChannelID string `gorm:"type:char(26);column:channel_id" json:"channel_id,omitempty"`

	// PrivacyMode names the privacy tier the shard satisfies. The
	// generator drops any chunk whose privacy_label is stricter
	// than this mode before emitting the shard.
	PrivacyMode string `gorm:"type:varchar(32);not null;column:privacy_mode" json:"privacy_mode"`

	// ShardVersion is monotonic per (tenant, user, channel,
	// privacy_mode) tuple. Clients ask for /delta?since=<version>
	// to discover what changed.
	ShardVersion int64 `gorm:"not null;default:1;column:shard_version" json:"shard_version"`

	// ChunksCount is the number of chunks in this shard. Cached on
	// the manifest so the API can return a cheap count.
	ChunksCount int `gorm:"not null;default:0;column:chunks_count" json:"chunks_count"`

	// Status is the lifecycle marker (see ShardStatus*).
	Status ShardStatus `gorm:"type:varchar(16);not null;default:'pending';column:status" json:"status"`

	CreatedAt time.Time `gorm:"not null;column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null;column:updated_at" json:"updated_at"`
}

// TableName overrides the default GORM pluralisation.
func (ShardManifest) TableName() string { return "shards" }

// Validate returns ErrInvalidManifest when required fields are
// missing or malformed. The repository calls this before INSERT.
func (m *ShardManifest) Validate() error {
	if m.ID == "" {
		return errors.New("shard: ID required")
	}
	if m.TenantID == "" {
		return errors.New("shard: TenantID required")
	}
	if m.PrivacyMode == "" {
		return errors.New("shard: PrivacyMode required")
	}
	if m.ShardVersion <= 0 {
		return errors.New("shard: ShardVersion must be > 0")
	}
	switch m.Status {
	case ShardStatusPending, ShardStatusReady, ShardStatusFailed, ShardStatusSuperseded:
		// ok
	case "":
		m.Status = ShardStatusPending
	default:
		return errors.New("shard: unknown Status")
	}
	return nil
}

// ShardChunk is the GORM model for the shard_chunks join table.
type ShardChunk struct {
	ShardID  string `gorm:"type:char(26);primaryKey;column:shard_id" json:"shard_id"`
	ChunkID  string `gorm:"type:varchar(128);primaryKey;column:chunk_id" json:"chunk_id"`
	TenantID string `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
}

// TableName overrides the default GORM pluralisation.
func (ShardChunk) TableName() string { return "shard_chunks" }

// TenantLifecycle is the GORM model for the tenant_lifecycle table
// used by the cryptographic forgetting flow.
type TenantLifecycle struct {
	TenantID    string         `gorm:"type:char(26);primaryKey;column:tenant_id" json:"tenant_id"`
	State       LifecycleState `gorm:"type:varchar(32);not null;column:state" json:"state"`
	RequestedBy string         `gorm:"column:requested_by" json:"requested_by,omitempty"`
	RequestedAt time.Time      `gorm:"not null;column:requested_at" json:"requested_at"`
	DeletedAt   *time.Time     `gorm:"column:deleted_at" json:"deleted_at,omitempty"`
}

// TableName overrides the default GORM pluralisation.
func (TenantLifecycle) TableName() string { return "tenant_lifecycle" }
