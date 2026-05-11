package admin

// chunk_quality_gorm.go — Round-9 Task 5.
//
// Postgres-backed ChunkQualityStore. The schema lives in
// migrations/028_chunk_quality.sql (table: chunk_quality, PK on
// (tenant_id, chunk_id)). The store carries the same shape as the
// existing ChunkQualityRow model so the handler/test surface keeps
// working unchanged.

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChunkQualityStoreGORM is the Postgres-backed implementation. It
// satisfies both the ChunkQualityReader interface (read path) and
// exposes Insert for pipeline writers — matching the
// InMemoryChunkQualityStore surface.
type ChunkQualityStoreGORM struct {
	db  *gorm.DB
	now func() time.Time
}

// NewChunkQualityStoreGORM validates inputs.
func NewChunkQualityStoreGORM(db *gorm.DB) (*ChunkQualityStoreGORM, error) {
	if db == nil {
		return nil, errors.New("chunk_quality_gorm: nil db")
	}
	return &ChunkQualityStoreGORM{db: db, now: time.Now}, nil
}

// AutoMigrate provisions the table when the SQL migration has not
// yet been applied (used by tests against SQLite).
func (s *ChunkQualityStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&ChunkQualityRow{})
}

// Insert persists a single chunk-quality row. Idempotent: a second
// insert for the same (tenant_id, chunk_id) overwrites the scores
// in place so the pre-write hook can be re-run without leaving
// stale rows.
func (s *ChunkQualityStoreGORM) Insert(ctx context.Context, row ChunkQualityRow) error {
	if row.TenantID == "" || row.ChunkID == "" {
		return errors.New("chunk_quality_gorm: missing tenant/chunk id")
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = s.now().UTC()
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "chunk_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"source_id", "document_id",
			"quality_score", "length_score", "lang_score", "embed_score",
			"duplicate", "updated_at",
		}),
	}).Create(&row).Error
}

// ListByTenant returns rows for the tenant. `limit` caps the row
// count (defaults to 10000 to match the handler's call).
func (s *ChunkQualityStoreGORM) ListByTenant(ctx context.Context, tenantID string, limit int) ([]ChunkQualityRow, error) {
	if tenantID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50000 {
		limit = 10000
	}
	var rows []ChunkQualityRow
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("source_id ASC, chunk_id ASC").
		Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
