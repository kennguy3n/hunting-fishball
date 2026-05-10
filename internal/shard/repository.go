package shard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// ErrShardNotFound is returned by Get / GetByVersion when no row
// matches the supplied scope. Cross-tenant lookups also surface this
// error so callers can't distinguish a tenant-leak attempt from a
// genuine miss.
var ErrShardNotFound = errors.New("shard: manifest not found")

// ErrMissingTenantScope is returned by every repository method when
// tenant_id is empty. Mirrors storage.ErrMissingTenantScope and
// guarantees a tenant filter is applied before any query runs.
var ErrMissingTenantScope = errors.New("shard: missing tenant_scope")

// Repository is the read/write port for the shards / shard_chunks /
// tenant_lifecycle tables. The struct is stateless apart from its
// *gorm.DB; all methods enforce tenant scope.
type Repository struct {
	db *gorm.DB
}

// NewRepository wires a Repository to the supplied *gorm.DB.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// ScopeFilter narrows List to a (tenant, user, channel) tuple.
// TenantID is mandatory; UserID and ChannelID are optional. An empty
// UserID returns shards visible to any user in the tenant; an empty
// ChannelID returns shards across every channel.
type ScopeFilter struct {
	TenantID  string
	UserID    string
	ChannelID string

	// Status, when non-empty, narrows to shards in that lifecycle
	// state (e.g. "ready" for the client-visible enumeration).
	Status ShardStatus

	// PrivacyMode, when non-empty, narrows to a specific tier.
	PrivacyMode string
}

// Create inserts a new manifest row. Mints a fresh ULID for the ID
// when one isn't supplied. Refuses to write without a tenant scope.
func (r *Repository) Create(ctx context.Context, m *ShardManifest) error {
	if m.TenantID == "" {
		return ErrMissingTenantScope
	}
	if m.ID == "" {
		m.ID = ulid.Make().String()
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if err := m.Validate(); err != nil {
		return err
	}

	return r.db.WithContext(ctx).Create(m).Error
}

// List returns every manifest matching the filter, newest-first.
// PageSize bounds the result set; <= 0 falls back to defaultPageSize.
func (r *Repository) List(ctx context.Context, f ScopeFilter, pageSize int) ([]ShardManifest, error) {
	if f.TenantID == "" {
		return nil, ErrMissingTenantScope
	}
	if pageSize <= 0 {
		pageSize = 50
	}

	q := r.db.WithContext(ctx).Where("tenant_id = ?", f.TenantID)
	if f.UserID != "" {
		q = q.Where("user_id = ?", f.UserID)
	}
	if f.ChannelID != "" {
		q = q.Where("channel_id = ?", f.ChannelID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", string(f.Status))
	}
	if f.PrivacyMode != "" {
		q = q.Where("privacy_mode = ?", f.PrivacyMode)
	}
	q = q.Order("shard_version DESC").Limit(pageSize)

	var rows []ShardManifest
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("shard: list: %w", err)
	}
	return rows, nil
}

// Get returns the manifest with id under the tenant scope. Returns
// ErrShardNotFound when the row is absent or owned by a different
// tenant.
func (r *Repository) Get(ctx context.Context, tenantID, id string) (*ShardManifest, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	var m ShardManifest
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrShardNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("shard: get: %w", err)
	}
	return &m, nil
}

// GetByVersion returns the manifest for an exact (tenant, user,
// channel, privacy_mode, version) tuple. Returns ErrShardNotFound
// when no row matches.
func (r *Repository) GetByVersion(ctx context.Context, f ScopeFilter, version int64) (*ShardManifest, error) {
	if f.TenantID == "" {
		return nil, ErrMissingTenantScope
	}
	q := r.db.WithContext(ctx).
		Where("tenant_id = ? AND privacy_mode = ? AND shard_version = ?", f.TenantID, f.PrivacyMode, version)
	if f.UserID != "" {
		q = q.Where("user_id = ?", f.UserID)
	} else {
		q = q.Where("(user_id IS NULL OR user_id = ?)", "")
	}
	if f.ChannelID != "" {
		q = q.Where("channel_id = ?", f.ChannelID)
	} else {
		q = q.Where("(channel_id IS NULL OR channel_id = ?)", "")
	}

	var m ShardManifest
	err := q.First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrShardNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("shard: get by version: %w", err)
	}
	return &m, nil
}

// LatestVersion returns the highest shard_version for the supplied
// scope, or 0 if no manifest exists yet. The next generator run
// uses this + 1.
func (r *Repository) LatestVersion(ctx context.Context, f ScopeFilter) (int64, error) {
	if f.TenantID == "" {
		return 0, ErrMissingTenantScope
	}
	q := r.db.WithContext(ctx).
		Model(&ShardManifest{}).
		Where("tenant_id = ? AND privacy_mode = ?", f.TenantID, f.PrivacyMode)
	if f.UserID != "" {
		q = q.Where("user_id = ?", f.UserID)
	} else {
		q = q.Where("(user_id IS NULL OR user_id = ?)", "")
	}
	if f.ChannelID != "" {
		q = q.Where("channel_id = ?", f.ChannelID)
	} else {
		q = q.Where("(channel_id IS NULL OR channel_id = ?)", "")
	}

	var version int64
	row := q.Select("COALESCE(MAX(shard_version), 0)").Row()
	if err := row.Scan(&version); err != nil {
		return 0, fmt.Errorf("shard: latest version: %w", err)
	}
	return version, nil
}

// SetChunkIDs replaces the chunk-ID set for a shard. The repository
// runs the delete + insert in a transaction so concurrent readers
// never see a partial set.
func (r *Repository) SetChunkIDs(ctx context.Context, tenantID, shardID string, chunkIDs []string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND shard_id = ?", tenantID, shardID).
			Delete(&ShardChunk{}).Error; err != nil {
			return fmt.Errorf("shard: clear chunks: %w", err)
		}
		if len(chunkIDs) == 0 {
			return nil
		}
		rows := make([]ShardChunk, 0, len(chunkIDs))
		for _, id := range chunkIDs {
			rows = append(rows, ShardChunk{
				ShardID:  shardID,
				ChunkID:  id,
				TenantID: tenantID,
			})
		}
		if err := tx.CreateInBatches(rows, 256).Error; err != nil {
			return fmt.Errorf("shard: insert chunks: %w", err)
		}
		return nil
	})
}

// ChunkIDs returns the chunk-ID set for a shard, sorted ascending
// so delta computation can stream both sides without materialising
// either fully.
func (r *Repository) ChunkIDs(ctx context.Context, tenantID, shardID string) ([]string, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	var ids []string
	err := r.db.WithContext(ctx).
		Model(&ShardChunk{}).
		Where("tenant_id = ? AND shard_id = ?", tenantID, shardID).
		Order("chunk_id ASC").
		Pluck("chunk_id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("shard: chunk ids: %w", err)
	}
	return ids, nil
}

// MarkReady transitions a manifest from pending to ready and writes
// the chunk count.
func (r *Repository) MarkReady(ctx context.Context, tenantID, shardID string, chunksCount int) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	res := r.db.WithContext(ctx).
		Model(&ShardManifest{}).
		Where("tenant_id = ? AND id = ?", tenantID, shardID).
		Updates(map[string]any{
			"status":       string(ShardStatusReady),
			"chunks_count": chunksCount,
			"updated_at":   time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("shard: mark ready: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrShardNotFound
	}
	return nil
}

// MarkSuperseded flips every prior version of (tenant, user, channel,
// privacy_mode) to superseded so the API surface only returns the
// freshest manifest by default.
func (r *Repository) MarkSuperseded(ctx context.Context, f ScopeFilter, currentVersion int64) error {
	if f.TenantID == "" {
		return ErrMissingTenantScope
	}
	q := r.db.WithContext(ctx).
		Model(&ShardManifest{}).
		Where("tenant_id = ? AND privacy_mode = ? AND shard_version < ? AND status = ?",
			f.TenantID, f.PrivacyMode, currentVersion, string(ShardStatusReady))
	if f.UserID != "" {
		q = q.Where("user_id = ?", f.UserID)
	} else {
		q = q.Where("(user_id IS NULL OR user_id = ?)", "")
	}
	if f.ChannelID != "" {
		q = q.Where("channel_id = ?", f.ChannelID)
	} else {
		q = q.Where("(channel_id IS NULL OR channel_id = ?)", "")
	}
	if err := q.Updates(map[string]any{
		"status":     string(ShardStatusSuperseded),
		"updated_at": time.Now().UTC(),
	}).Error; err != nil {
		return fmt.Errorf("shard: mark superseded: %w", err)
	}
	return nil
}

// MarkFailed transitions a manifest to the failed state. Used by the
// generator when retries are exhausted.
func (r *Repository) MarkFailed(ctx context.Context, tenantID, shardID string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	res := r.db.WithContext(ctx).
		Model(&ShardManifest{}).
		Where("tenant_id = ? AND id = ?", tenantID, shardID).
		Updates(map[string]any{
			"status":     string(ShardStatusFailed),
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("shard: mark failed: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrShardNotFound
	}
	return nil
}

// MarkLifecycle upserts a tenant_lifecycle row. Used by the
// cryptographic forgetting flow.
func (r *Repository) MarkLifecycle(ctx context.Context, tenantID string, state LifecycleState, requestedBy string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	now := time.Now().UTC()
	row := TenantLifecycle{
		TenantID:    tenantID,
		State:       state,
		RequestedBy: requestedBy,
		RequestedAt: now,
	}
	if state == LifecycleDeleted {
		row.DeletedAt = &now
	}

	return r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Assign(map[string]any{
			"state":        string(state),
			"requested_by": requestedBy,
			"deleted_at":   row.DeletedAt,
		}).
		FirstOrCreate(&row).Error
}

// LifecycleState returns the current state for tenantID, or empty
// string when no row exists.
func (r *Repository) LifecycleState(ctx context.Context, tenantID string) (LifecycleState, error) {
	if tenantID == "" {
		return "", ErrMissingTenantScope
	}
	var row TenantLifecycle
	err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("shard: lifecycle state: %w", err)
	}
	return row.State, nil
}

// AutoMigrate creates the tables in dev / test backends. Production
// migrations live in migrations/006_shards.sql.
func (r *Repository) AutoMigrate(ctx context.Context) error {
	return r.db.WithContext(ctx).AutoMigrate(&ShardManifest{}, &ShardChunk{}, &TenantLifecycle{})
}
