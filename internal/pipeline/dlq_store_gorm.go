package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// DLQStoreGORM is the *gorm.DB-backed DLQStore wired in cmd/ingest
// and cmd/api. The schema lives in migrations/009_dlq_messages.sql.
type DLQStoreGORM struct {
	db *gorm.DB
}

// NewDLQStoreGORM returns a DLQStoreGORM bound to db.
func NewDLQStoreGORM(db *gorm.DB) *DLQStoreGORM {
	return &DLQStoreGORM{db: db}
}

// AutoMigrate creates the table when the SQL migration hasn't been
// applied yet. Safe to call multiple times.
func (s *DLQStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&DLQMessage{})
}

// ErrDLQNotFound is returned by Get + MarkReplayed when no row
// matches (tenant_id, id). Cross-tenant lookups also surface this
// error so the admin handler returns 404 rather than 500.
var ErrDLQNotFound = errors.New("dlq: message not found")

// Insert persists a single row.
func (s *DLQStoreGORM) Insert(ctx context.Context, msg *DLQMessage) error {
	if msg == nil {
		return errors.New("dlq store: nil message")
	}
	if msg.TenantID == "" {
		return errors.New("dlq store: missing tenant_id")
	}
	return s.db.WithContext(ctx).Create(msg).Error
}

// List returns up to EffectiveDLQPageSize(filter.PageSize)+1 rows
// for the supplied tenant. The trailing +1 is the standard
// fetch-N+1 pagination probe: the admin handler emits a
// next_page_token only when the store actually returned more rows
// than the caller asked for, avoiding the phantom-token regression
// where every full page (or every non-empty response under the
// default page size) suggested another page existed.
//
// Cursor pagination is keyed on id (ULIDs are time-ordered) so the
// admin portal can scroll cheaply.
func (s *DLQStoreGORM) List(ctx context.Context, filter DLQListFilter) ([]DLQMessage, error) {
	if filter.TenantID == "" {
		return nil, errors.New("dlq store: missing tenant_id")
	}
	pageSize := EffectiveDLQPageSize(filter.PageSize)
	q := s.db.WithContext(ctx).
		Where("tenant_id = ?", filter.TenantID).
		Order("id DESC").
		Limit(pageSize + 1)

	if filter.OriginalTopic != "" {
		q = q.Where("original_topic = ?", filter.OriginalTopic)
	}
	if filter.SourceID != "" {
		q = q.Where("source_id = ?", filter.SourceID)
	}
	if !filter.MinCreatedAt.IsZero() {
		q = q.Where("created_at >= ?", filter.MinCreatedAt)
	}
	if !filter.MaxCreatedAt.IsZero() {
		q = q.Where("created_at < ?", filter.MaxCreatedAt)
	}
	if filter.Category != "" {
		q = q.Where("category = ?", filter.Category)
	}
	if !filter.IncludeReplayed {
		q = q.Where("replayed_at IS NULL")
	}
	if filter.PageToken != "" {
		q = q.Where("id < ?", filter.PageToken)
	}

	var rows []DLQMessage
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("dlq store: list: %w", err)
	}
	return rows, nil
}

// Get returns a single row scoped to tenantID.
func (s *DLQStoreGORM) Get(ctx context.Context, tenantID, id string) (*DLQMessage, error) {
	if tenantID == "" || id == "" {
		return nil, ErrDLQNotFound
	}
	var row DLQMessage
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDLQNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("dlq store: get: %w", err)
	}
	return &row, nil
}

// MarkReplayed stamps replayed_at + replay_error on the row.
// replayErr nil clears any previous error.
func (s *DLQStoreGORM) MarkReplayed(ctx context.Context, id string, replayErr error) error {
	if id == "" {
		return ErrDLQNotFound
	}
	updates := map[string]any{
		"replayed_at":  time.Now().UTC(),
		"replay_error": "",
	}
	if replayErr != nil {
		updates["replay_error"] = replayErr.Error()
	}
	res := s.db.WithContext(ctx).
		Model(&DLQMessage{}).
		Where("id = ?", id).
		Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("dlq store: mark replayed: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrDLQNotFound
	}
	return nil
}

// BumpAttemptCount increments attempt_count by one and returns the
// new value.
func (s *DLQStoreGORM) BumpAttemptCount(ctx context.Context, id string) (int, error) {
	if id == "" {
		return 0, ErrDLQNotFound
	}
	res := s.db.WithContext(ctx).
		Model(&DLQMessage{}).
		Where("id = ?", id).
		UpdateColumn("attempt_count", gorm.Expr("attempt_count + 1"))
	if res.Error != nil {
		return 0, fmt.Errorf("dlq store: bump: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return 0, ErrDLQNotFound
	}
	var row DLQMessage
	if err := s.db.WithContext(ctx).
		Select("attempt_count").
		Where("id = ?", id).
		First(&row).Error; err != nil {
		return 0, fmt.Errorf("dlq store: read after bump: %w", err)
	}
	return row.AttemptCount, nil
}
