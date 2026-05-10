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
	"gorm.io/gorm/clause"
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

	// Atomic upsert. The previous shape was check-then-act (UPDATE
	// first, fall back to CREATE when RowsAffected==0); two goroutines
	// hitting the same (tenant, source, namespace) triple on its first
	// insert would both see RowsAffected==0 and both attempt CREATE —
	// the second hits the primary-key constraint on Postgres. Using
	// `INSERT ... ON CONFLICT (...) DO UPDATE SET col = col +
	// excluded.col, updated_at = ?` collapses both halves into a
	// single statement that is atomic at the database level on both
	// Postgres and SQLite (≥3.24, which is what glebarez/sqlite ships).
	//
	// `excluded.<col>` is the standard SQL alias for the row that
	// would have been inserted on conflict; for our INSERT it equals
	// `delta`, so the resulting expression is `column + delta` — the
	// same arithmetic as the prior UpdateColumns path, but without the
	// race window.
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "tenant_id"},
			{Name: "source_id"},
			{Name: "namespace_id"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			column:       gorm.Expr(column + " + excluded." + column),
			"updated_at": now,
		}),
	}).Create(&row).Error
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
