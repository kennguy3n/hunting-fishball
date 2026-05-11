package admin

// pinned_results_gorm.go — Round-8 Task 7.
//
// Postgres-backed PinnedResultStore. Schema lives in
// migrations/031_pinned_results.sql (table:
// pinned_retrieval_results). The Lookup path uses exact pattern
// match so the retrieval handler can resolve a query → pins in O(1)
// indexed reads on (tenant_id, query_pattern).

import (
	"context"
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"
)

// pinnedRow is the GORM row representation. Kept private — the
// public PinnedResult shape is the one the API surface exports.
type pinnedRow struct {
	ID           string    `gorm:"primaryKey;column:id;type:char(26)"`
	TenantID     string    `gorm:"column:tenant_id;type:char(26);not null;index:idx_pinned_results_tenant_pattern,priority:1"`
	QueryPattern string    `gorm:"column:query_pattern;type:text;not null;index:idx_pinned_results_tenant_pattern,priority:2"`
	ChunkID      string    `gorm:"column:chunk_id;type:varchar(128);not null"`
	Position     int       `gorm:"column:position;not null;default:0"`
	CreatedAt    time.Time `gorm:"column:created_at;not null;default:now()"`
}

// TableName binds the GORM model to the migration's table.
func (pinnedRow) TableName() string { return "pinned_retrieval_results" }

// PinnedResultStoreGORM is the Postgres implementation.
type PinnedResultStoreGORM struct {
	db   *gorm.DB
	idFn func() string
}

// NewPinnedResultStoreGORM constructs the GORM-backed store.
func NewPinnedResultStoreGORM(db *gorm.DB) (*PinnedResultStoreGORM, error) {
	if db == nil {
		return nil, errors.New("pinned_results: nil db")
	}
	return &PinnedResultStoreGORM{db: db, idFn: newPinID}, nil
}

// AutoMigrate provisions the table when the migration files have
// not yet been applied (used by tests).
func (s *PinnedResultStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&pinnedRow{})
}

// Lookup returns pins whose QueryPattern matches `query` exactly,
// ordered by Position ascending.
func (s *PinnedResultStoreGORM) Lookup(ctx context.Context, tenantID, query string) ([]PinnedResult, error) {
	if tenantID == "" {
		return nil, nil
	}
	var rows []pinnedRow
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND query_pattern = ?", tenantID, query).
		Order("position ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PinnedResult, 0, len(rows))
	for _, r := range rows {
		out = append(out, toPinnedResult(r))
	}
	return out, nil
}

// List returns every pin for the tenant.
func (s *PinnedResultStoreGORM) List(ctx context.Context, tenantID string) ([]PinnedResult, error) {
	if tenantID == "" {
		return nil, nil
	}
	var rows []pinnedRow
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PinnedResult, 0, len(rows))
	for _, r := range rows {
		out = append(out, toPinnedResult(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].QueryPattern == out[j].QueryPattern {
			return out[i].Position < out[j].Position
		}
		return out[i].QueryPattern < out[j].QueryPattern
	})
	return out, nil
}

// Create inserts a new pin row.
func (s *PinnedResultStoreGORM) Create(ctx context.Context, p PinnedResult) (PinnedResult, error) {
	if p.TenantID == "" || p.QueryPattern == "" || p.ChunkID == "" {
		return PinnedResult{}, errors.New("pinned_results: missing required field")
	}
	if p.ID == "" {
		p.ID = s.idFn()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	row := pinnedRow{
		ID: p.ID, TenantID: p.TenantID, QueryPattern: p.QueryPattern,
		ChunkID: p.ChunkID, Position: p.Position, CreatedAt: p.CreatedAt,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return PinnedResult{}, err
	}
	return p, nil
}

// Delete removes the pin row by id, scoped to tenant.
func (s *PinnedResultStoreGORM) Delete(ctx context.Context, tenantID, id string) error {
	if tenantID == "" || id == "" {
		return errors.New("pinned_results: missing id or tenant")
	}
	res := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&pinnedRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("pinned_results: not found")
	}
	return nil
}

func toPinnedResult(r pinnedRow) PinnedResult {
	return PinnedResult{
		ID: r.ID, TenantID: r.TenantID, QueryPattern: r.QueryPattern,
		ChunkID: r.ChunkID, Position: r.Position, CreatedAt: r.CreatedAt,
	}
}
