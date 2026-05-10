package shard

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// PostgresChunkSource adapts *storage.PostgresStore to the
// generator's ChunkSource interface. It walks the chunks table and
// projects each row down to ChunkScope.
//
// The query is tenant-scoped at the storage boundary; cross-tenant
// reads are structurally impossible.
type PostgresChunkSource struct {
	db *gorm.DB
}

// NewPostgresChunkSource wires a PostgresChunkSource to the supplied
// *gorm.DB. Returns an error if db is nil.
func NewPostgresChunkSource(db *gorm.DB) (*PostgresChunkSource, error) {
	if db == nil {
		return nil, errors.New("shard: nil db")
	}
	return &PostgresChunkSource{db: db}, nil
}

// ListChunks implements ChunkSource. Returns every chunk for
// tenantID, projected to ChunkScope. The output is sorted by
// chunk_id so the generator's downstream sort is a no-op when the
// underlying index already orders by id.
func (p *PostgresChunkSource) ListChunks(ctx context.Context, tenantID string) ([]ChunkScope, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	var rows []storage.Chunk
	err := p.db.WithContext(ctx).
		Model(&storage.Chunk{}).
		Where("tenant_id = ?", tenantID).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("shard: postgres chunk source: %w", err)
	}

	out := make([]ChunkScope, 0, len(rows))
	for _, c := range rows {
		out = append(out, ChunkScope{
			ChunkID:      c.ID,
			DocumentID:   c.DocumentID,
			NamespaceID:  c.NamespaceID,
			SourceID:     c.SourceID,
			URI:          c.URI,
			PrivacyLabel: c.PrivacyLabel,
			Connector:    c.Connector,
		})
	}
	return out, nil
}
