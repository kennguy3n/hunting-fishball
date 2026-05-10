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

// ErrSourceTerminal is returned by Update when the row exists but is
// in a terminal status (removing/removed) where mutation would race
// with the forget worker. Handlers translate this to HTTP 409.
var ErrSourceTerminal = errors.New("admin: source is in a terminal status")

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
	// PageSize is the soft cap on returned rows (default 50, max
	// 200). Mirrors the audit / DLQ pagination shape so callers
	// can use the same client logic across the admin surface.
	PageSize int
	// Cursor is the opaque page-cursor returned by the previous
	// call's NextCursor (empty for the first page). For the
	// sources list we use the row id directly: rows are ordered
	// id DESC, so a cursor of "01XYZ" returns id < "01XYZ".
	Cursor string
}

// ListResult bundles a page of rows with the cursor a caller passes
// back to fetch the next page. Empty NextCursor means no more pages.
type ListResult struct {
	Items      []Source
	NextCursor string
}

// effectivePageSize clamps PageSize to the documented [1, 200] range
// and falls back to 50 when unset. Exposed for handler-side reuse.
func effectivePageSize(requested int) int {
	if requested <= 0 {
		return 50
	}
	if requested > 200 {
		return 200
	}
	return requested
}

// List returns one page of sources for tenantID, ordered by id DESC,
// with a cursor for the subsequent page.
//
// The cursor is the id of the last row on the current page. Reads
// `pageSize+1` rows so we can detect whether another page exists
// without an extra COUNT(*) round-trip.
func (r *SourceRepository) List(ctx context.Context, f ListFilter) (ListResult, error) {
	if f.TenantID == "" {
		return ListResult{}, errors.New("admin: ListFilter.TenantID is required")
	}
	pageSize := effectivePageSize(f.PageSize)
	q := r.db.WithContext(ctx).
		Where("tenant_id = ?", f.TenantID).
		Order("id DESC").
		Limit(pageSize + 1)
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Cursor != "" {
		q = q.Where("id < ?", f.Cursor)
	}
	var out []Source
	if err := q.Find(&out).Error; err != nil {
		return ListResult{}, fmt.Errorf("admin: list sources: %w", err)
	}
	res := ListResult{Items: out}
	if len(out) > pageSize {
		res.Items = out[:pageSize]
		res.NextCursor = res.Items[len(res.Items)-1].ID
	}
	return res, nil
}

// UpdatePatch describes the subset of fields a PATCH request may
// change. Nil pointers are ignored.
type UpdatePatch struct {
	Status *SourceStatus
	Scopes *[]string
}

// Update applies a patch to a source row and returns the refreshed
// row. The update is scoped to (tenant_id, id) so cross-tenant
// patches are structurally impossible. Rows already in a terminal
// status (removing/removed) are not patchable: ErrSourceTerminal is
// returned so the forget worker's status check at
// internal/admin/forget_worker.go cannot be raced by a concurrent
// PATCH that flips the row back to active.
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

	terminal := []string{string(SourceStatusRemoving), string(SourceStatusRemoved)}
	res := r.db.WithContext(ctx).
		Model(&Source{}).
		Where("tenant_id = ? AND id = ? AND status NOT IN ?", tenantID, id, terminal).
		Updates(updates)
	if res.Error != nil {
		return nil, fmt.Errorf("admin: update source: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// The row was either missing or in a terminal status. Disambiguate
		// without a second WHERE clause so the caller can return 404 vs
		// 409 correctly.
		var existing Source
		lookupErr := r.db.WithContext(ctx).
			Select("status").
			Where("tenant_id = ? AND id = ?", tenantID, id).
			First(&existing).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return nil, ErrSourceNotFound
		}
		if lookupErr != nil {
			return nil, fmt.Errorf("admin: update source lookup: %w", lookupErr)
		}
		return nil, ErrSourceTerminal
	}

	return r.Get(ctx, tenantID, id)
}

// GetByID resolves a source by its primary key alone — without
// the usual tenant_id scoping. Used only by the inbound webhook
// router (Round-5 Task 5), where the upstream SaaS caller doesn't
// know which tenant the source belongs to. The handler
// re-derives the tenant from the returned row and never trusts
// any tenant value from the request.
func (r *SourceRepository) GetByID(ctx context.Context, id string) (*Source, error) {
	if id == "" {
		return nil, ErrSourceNotFound
	}
	var s Source
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("admin: get source by id: %w", err)
	}
	return &s, nil
}

// ListAllActive returns every active source across all tenants in
// id-ascending order. Used by background workers (token refresh,
// credential expiry monitor) that intentionally span tenants. The
// HTTP admin surface NEVER calls this — request-scoped reads must
// always go through List which enforces tenant scoping.
func (r *SourceRepository) ListAllActive(ctx context.Context) ([]Source, error) {
	var out []Source
	if err := r.db.WithContext(ctx).
		Where("status = ?", string(SourceStatusActive)).
		Order("id ASC").
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("admin: list all active sources: %w", err)
	}
	return out, nil
}

// UpdateConfig overwrites the JSONB config blob and bumps updated_at
// for (tenant_id, id). Used by token refresh and credential expiry
// workers that need to write back the refreshed access_token without
// changing any other column on the row.
func (r *SourceRepository) UpdateConfig(ctx context.Context, tenantID, id string, cfg JSONMap) error {
	if tenantID == "" || id == "" {
		return ErrSourceNotFound
	}
	res := r.db.WithContext(ctx).
		Model(&Source{}).
		Where("tenant_id = ? AND id = ? AND status NOT IN ?", tenantID, id, []string{string(SourceStatusRemoving), string(SourceStatusRemoved)}).
		Updates(map[string]any{"config": cfg, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return fmt.Errorf("admin: update source config: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSourceNotFound
	}
	return nil
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
