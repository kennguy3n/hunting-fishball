// sync_progress.go — Phase 8 / Task 14 sync progress tracking.
//
// docs/PROPOSAL.md §5 promises "Progress is reported per namespace"
// during initial sync. This file persists per (tenant_id, source_id,
// namespace_id) counters that the pipeline coordinator increments at
// Stage-1 enumeration ("discovered") and Stage-4 store completion
// ("processed"), so an admin UI can render an accurate progress bar
// without polling Kafka.
package admin

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// SyncProgress is the GORM row backing the progress table.
type SyncProgress struct {
	TenantID    string    `gorm:"column:tenant_id;primaryKey;type:char(26)"`
	SourceID    string    `gorm:"column:source_id;primaryKey;type:char(26)"`
	NamespaceID string    `gorm:"column:namespace_id;primaryKey;type:varchar(256)"`
	Discovered  int64     `gorm:"column:discovered"`
	Processed   int64     `gorm:"column:processed"`
	Failed      int64     `gorm:"column:failed"`
	StartedAt   time.Time `gorm:"column:started_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName overrides the default plural form.
func (SyncProgress) TableName() string { return "sync_progress" }

// SyncProgressStore is the read/write port the pipeline coordinator
// and the admin handler share.
type SyncProgressStore interface {
	IncrementDiscovered(ctx context.Context, tenantID, sourceID, namespaceID string, delta int64) error
	IncrementProcessed(ctx context.Context, tenantID, sourceID, namespaceID string, delta int64) error
	IncrementFailed(ctx context.Context, tenantID, sourceID, namespaceID string, delta int64) error
	List(ctx context.Context, tenantID, sourceID string) ([]SyncProgress, error)
}

// NewSyncProgressStoreGORM returns a GORM-backed store. AutoMigrate
// must be called once before first use; cmd/api wires that into the
// startup sequence.
func NewSyncProgressStoreGORM(db *gorm.DB) *SyncProgressStoreGORM {
	return &SyncProgressStoreGORM{db: db}
}

// SyncProgressStoreGORM is the production *gorm.DB implementation.
type SyncProgressStoreGORM struct {
	db *gorm.DB
	// nowFn is overridable in tests so the started_at/updated_at
	// columns are deterministic.
	nowFn func() time.Time
}

// AutoMigrate creates the table if it doesn't exist.
func (s *SyncProgressStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&SyncProgress{})
}

func (s *SyncProgressStoreGORM) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now().UTC()
}

func (s *SyncProgressStoreGORM) increment(ctx context.Context, tenantID, sourceID, namespaceID, column string, delta int64) error {
	if tenantID == "" || sourceID == "" {
		return errors.New("sync progress: missing tenant/source")
	}
	if delta == 0 {
		return nil
	}
	switch column {
	case "discovered", "processed", "failed":
	default:
		return errors.New("sync progress: unknown column")
	}
	now := s.now()
	tx := s.db.WithContext(ctx).Model(&SyncProgress{}).
		Where("tenant_id = ? AND source_id = ? AND namespace_id = ?", tenantID, sourceID, namespaceID).
		UpdateColumns(map[string]any{
			column:       gorm.Expr(column+" + ?", delta),
			"updated_at": now,
		})
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected > 0 {
		return nil
	}
	row := SyncProgress{
		TenantID:    tenantID,
		SourceID:    sourceID,
		NamespaceID: namespaceID,
		StartedAt:   now,
		UpdatedAt:   now,
	}
	switch column {
	case "discovered":
		row.Discovered = delta
	case "processed":
		row.Processed = delta
	case "failed":
		row.Failed = delta
	}
	return s.db.WithContext(ctx).Create(&row).Error
}

// IncrementDiscovered bumps the discovered counter by delta.
func (s *SyncProgressStoreGORM) IncrementDiscovered(ctx context.Context, tenantID, sourceID, namespaceID string, delta int64) error {
	return s.increment(ctx, tenantID, sourceID, namespaceID, "discovered", delta)
}

// IncrementProcessed bumps the processed counter by delta.
func (s *SyncProgressStoreGORM) IncrementProcessed(ctx context.Context, tenantID, sourceID, namespaceID string, delta int64) error {
	return s.increment(ctx, tenantID, sourceID, namespaceID, "processed", delta)
}

// IncrementFailed bumps the failed counter by delta.
func (s *SyncProgressStoreGORM) IncrementFailed(ctx context.Context, tenantID, sourceID, namespaceID string, delta int64) error {
	return s.increment(ctx, tenantID, sourceID, namespaceID, "failed", delta)
}

// List returns all rows for (tenantID, sourceID).
func (s *SyncProgressStoreGORM) List(ctx context.Context, tenantID, sourceID string) ([]SyncProgress, error) {
	if tenantID == "" || sourceID == "" {
		return nil, errors.New("sync progress: missing tenant/source")
	}
	var rows []SyncProgress
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		Order("namespace_id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
