package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrSourceNotFound is returned by Get when no row matches the
// (tenant_id, id) tuple.
var ErrSourceNotFound = errors.New("admin: source not found")

// SourceRepository is the read/write port for the sources table. All
// methods are tenant-scoped: a misconfigured caller cannot pull
// another tenant's row.
type SourceRepository struct {
	db *gorm.DB
}

// NewSourceRepository wires a SourceRepository to the supplied
// *gorm.DB.
func NewSourceRepository(db *gorm.DB) *SourceRepository {
	return &SourceRepository{db: db}
}

// DB exposes the underlying handle so handlers can run audit-log
// inserts inside the same transaction as the source write.
func (r *SourceRepository) DB() *gorm.DB { return r.db }

// Create inserts a new source row.
func (r *SourceRepository) Create(ctx context.Context, s *Source) error {
	return r.CreateInTx(ctx, r.db, s)
}

// CreateInTx inserts a new source row inside an externally-managed
// transaction. Used by the admin handler so the source row + the
// audit row commit atomically.
func (r *SourceRepository) CreateInTx(ctx context.Context, tx *gorm.DB, s *Source) error {
	if s == nil {
		return errors.New("admin: nil source")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("admin: nil tx")
	}
	if err := tx.WithContext(ctx).Create(s).Error; err != nil {
		return fmt.Errorf("admin: insert source: %w", err)
	}
	return nil
}

// Get returns a single source row, scoped to tenantID. Cross-tenant
// lookups return ErrSourceNotFound — never the row.
func (r *SourceRepository) Get(ctx context.Context, tenantID, id string) (*Source, error) {
	if tenantID == "" || id == "" {
		return nil, ErrSourceNotFound
	}
	var s Source
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("admin: get source: %w", err)
	}
	return &s, nil
}

// ListFilter narrows ListSources. TenantID is mandatory.
type ListFilter struct {
	TenantID string
	Status   SourceStatus
	PageSize int
}

// List returns sources for tenantID, ordered by id DESC.
func (r *SourceRepository) List(ctx context.Context, f ListFilter) ([]Source, error) {
	if f.TenantID == "" {
		return nil, errors.New("admin: ListFilter.TenantID is required")
	}
	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}

	q := r.db.WithContext(ctx).
		Where("tenant_id = ?", f.TenantID).
		Order("id DESC").
		Limit(pageSize)
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	var out []Source
	if err := q.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("admin: list sources: %w", err)
	}
	return out, nil
}

// UpdatePatch describes the subset of fields a PATCH request may
// change. Nil pointers are ignored.
type UpdatePatch struct {
	Status *SourceStatus
	Scopes *[]string
}

// Update applies a patch to a source row and returns the refreshed
// row. The update is scoped to (tenant_id, id) so cross-tenant
// patches are structurally impossible.
func (r *SourceRepository) Update(ctx context.Context, tenantID, id string, patch UpdatePatch) (*Source, error) {
	if tenantID == "" || id == "" {
		return nil, ErrSourceNotFound
	}
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if patch.Status != nil {
		switch *patch.Status {
		case SourceStatusActive, SourceStatusPaused:
		default:
			return nil, fmt.Errorf("admin: status %q not patchable via Update", *patch.Status)
		}
		updates["status"] = string(*patch.Status)
	}
	if patch.Scopes != nil {
		updates["scopes"] = JSONStringSlice(*patch.Scopes)
	}

	res := r.db.WithContext(ctx).
		Model(&Source{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Updates(updates)
	if res.Error != nil {
		return nil, fmt.Errorf("admin: update source: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, ErrSourceNotFound
	}

	return r.Get(ctx, tenantID, id)
}

// MarkRemoving flips a source to status=removing. Idempotent — a
// source already in removing/removed returns its current row.
func (r *SourceRepository) MarkRemoving(ctx context.Context, tenantID, id string) (*Source, error) {
	if tenantID == "" || id == "" {
		return nil, ErrSourceNotFound
	}
	res := r.db.WithContext(ctx).
		Model(&Source{}).
		Where("tenant_id = ? AND id = ? AND status NOT IN ?", tenantID, id, []string{string(SourceStatusRemoving), string(SourceStatusRemoved)}).
		Updates(map[string]any{"status": string(SourceStatusRemoving), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return nil, fmt.Errorf("admin: mark removing: %w", res.Error)
	}
	return r.Get(ctx, tenantID, id)
}

// MarkRemoved flips a source from removing to removed. Used by the
// forget worker after derived data is purged.
func (r *SourceRepository) MarkRemoved(ctx context.Context, tenantID, id string) error {
	if tenantID == "" || id == "" {
		return ErrSourceNotFound
	}
	res := r.db.WithContext(ctx).
		Model(&Source{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Updates(map[string]any{"status": string(SourceStatusRemoved), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return fmt.Errorf("admin: mark removed: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSourceNotFound
	}
	return nil
}
