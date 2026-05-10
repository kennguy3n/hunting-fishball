package audit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrAuditLogNotFound is returned by Get when no row matches the
// (tenant_id, id) tuple.
var ErrAuditLogNotFound = errors.New("audit: log not found")

// ListFilter narrows ListAuditLogs. Zero-valued fields are ignored.
type ListFilter struct {
	// TenantID is mandatory — the repository refuses to run a query
	// that would cross tenant boundaries.
	TenantID string

	// Actions filters by exact match against any of the supplied actions.
	Actions []Action

	// ResourceType filters by exact match.
	ResourceType string

	// ResourceID filters by exact match. Phase 8 / Task 13 addition —
	// admin search/filter API binds `source_id=` to ResourceID for the
	// common case of "show me everything that touched this source".
	ResourceID string

	// Since/Until bracket created_at. Zero values are open-ended.
	Since time.Time
	Until time.Time

	// PayloadSearch is a substring matched against the JSONB-serialised
	// payload. Phase 8 / Task 13 adds rudimentary text search on top of
	// the existing exact-match filters; the comparison is dialect-agnostic
	// (LIKE ?) so SQLite + Postgres tests stay in sync.
	PayloadSearch string

	// PageSize is the LIMIT clause. <=0 falls back to defaultPageSize.
	PageSize int

	// PageToken is the opaque "before this ULID" cursor. Empty starts at
	// the most recent row.
	PageToken string
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// ListResult is the ListAuditLogs response.
type ListResult struct {
	Items         []AuditLog
	NextPageToken string
}

// Repository is the read/write port for the audit_logs table. The write
// path is the outbox: callers pass their own *gorm.DB so the audit row
// gets inserted in the same transaction as the business operation.
type Repository struct {
	db *gorm.DB
}

// NewRepository wires a Repository to the supplied *gorm.DB. The DB
// handle is the read default; CreateInTx accepts a separate tx handle.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Create inserts a single audit log row in its own transaction.
// Convenience wrapper; production paths should prefer CreateInTx so the
// audit row commits atomically with the business write.
func (r *Repository) Create(ctx context.Context, log *AuditLog) error {
	return r.CreateInTx(ctx, r.db, log)
}

// CreateInTx inserts the audit log inside an externally-managed
// transaction. This is the transactional-outbox entry point: callers run
// their business write and the audit insert under the same tx.WithContext.
func (r *Repository) CreateInTx(ctx context.Context, tx *gorm.DB, log *AuditLog) error {
	if log == nil {
		return errors.New("audit: nil log")
	}
	if err := log.Validate(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("audit: nil tx")
	}

	if err := tx.WithContext(ctx).Create(log).Error; err != nil {
		return fmt.Errorf("audit: insert log: %w", err)
	}

	return nil
}

// Get returns a single audit log entry, scoped to the supplied tenantID.
// Cross-tenant lookups return ErrAuditLogNotFound — never the row.
func (r *Repository) Get(ctx context.Context, tenantID, id string) (*AuditLog, error) {
	if tenantID == "" || id == "" {
		return nil, ErrAuditLogNotFound
	}

	var log AuditLog
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&log).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAuditLogNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("audit: get log: %w", err)
	}

	return &log, nil
}

// List returns a page of audit logs ordered by id DESC (newest first).
func (r *Repository) List(ctx context.Context, f ListFilter) (*ListResult, error) {
	if f.TenantID == "" {
		return nil, errors.New("audit: ListFilter.TenantID is required")
	}
	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	q := r.db.WithContext(ctx).
		Where("tenant_id = ?", f.TenantID).
		Order("id DESC").
		Limit(pageSize + 1)

	if len(f.Actions) > 0 {
		q = q.Where("action IN ?", f.Actions)
	}
	if f.ResourceType != "" {
		q = q.Where("resource_type = ?", f.ResourceType)
	}
	if f.ResourceID != "" {
		q = q.Where("resource_id = ?", f.ResourceID)
	}
	if f.PayloadSearch != "" {
		q = q.Where("payload LIKE ?", "%"+f.PayloadSearch+"%")
	}
	if !f.Since.IsZero() {
		q = q.Where("created_at >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		q = q.Where("created_at < ?", f.Until)
	}
	if f.PageToken != "" {
		q = q.Where("id < ?", f.PageToken)
	}

	var rows []AuditLog
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("audit: list logs: %w", err)
	}

	res := &ListResult{Items: rows}
	if len(rows) > pageSize {
		res.Items = rows[:pageSize]
		res.NextPageToken = rows[pageSize-1].ID
	}

	return res, nil
}

// PendingPublish returns up to limit audit rows that have not yet been
// published to Kafka. Used by the outbox poller.
func (r *Repository) PendingPublish(ctx context.Context, limit int) ([]AuditLog, error) {
	if limit <= 0 {
		limit = defaultPageSize
	}

	var rows []AuditLog
	err := r.db.WithContext(ctx).
		Where("published_at IS NULL").
		Order("id ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("audit: list pending: %w", err)
	}

	return rows, nil
}

// MarkPublished stamps published_at on a batch of rows.
func (r *Repository) MarkPublished(ctx context.Context, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	res := r.db.WithContext(ctx).
		Model(&AuditLog{}).
		Where("id IN ?", ids).
		Update("published_at", at)
	if res.Error != nil {
		return fmt.Errorf("audit: mark published: %w", res.Error)
	}

	return nil
}
