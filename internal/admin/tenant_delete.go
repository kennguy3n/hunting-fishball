// tenant_delete.go — admin-facing wrapper around the cryptographic-
// forget workflow documented in `docs/ARCHITECTURE.md` §5.
//
// The package implements the same five-step workflow the shard.Forget
// orchestrator runs against `DELETE /v1/tenants/:tenant_id/keys`,
// but exposes it under the admin surface (`DELETE
// /v1/admin/tenants/:tenant_id`) and additionally writes the
// denormalised `tenants.tenant_status` column added in
// migrations/008_tenant_status.sql so admin listings can show
// pending_deletion / deleted without joining the lifecycle table.
//
// Implementations of the five steps live in the underlying sweeper
// list — Qdrant, FalkorDB, Tantivy, Redis, DEK destroyer. The
// deleter is intentionally a thin orchestrator: it owns the
// state-machine transitions and audit emissions, the heavy lifting
// happens in the wired sweeper implementations.
package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// TenantStatus is the lifecycle state of a tenant in the admin
// directory table.
type TenantStatus string

const (
	TenantStatusActive          TenantStatus = "active"
	TenantStatusPendingDeletion TenantStatus = "pending_deletion"
	TenantStatusDeleted         TenantStatus = "deleted"
)

// TenantSweeper is one storage tier the deleter cleans on a full-
// tenant key destruction. Mirrors shard.TenantSweeper so a single
// adapter can satisfy both packages.
type TenantSweeper interface {
	ForgetTenant(ctx context.Context, tenantID string) error
}

// TenantSweeperFunc adapts a plain function to TenantSweeper.
type TenantSweeperFunc func(ctx context.Context, tenantID string) error

// ForgetTenant implements TenantSweeper.
func (f TenantSweeperFunc) ForgetTenant(ctx context.Context, tenantID string) error {
	return f(ctx, tenantID)
}

// TenantStatusStore persists the denormalised status column.
// Production wiring is *TenantStatusRepoGORM; tests inject a fake.
type TenantStatusStore interface {
	UpsertStatus(ctx context.Context, tenantID string, status TenantStatus) error
}

// TenantDeleterConfig configures a TenantDeleter.
type TenantDeleterConfig struct {
	// Status persists the denormalised tenant_status column.
	Status TenantStatusStore

	// Sweepers is the ordered list of storage tiers to sweep:
	// Qdrant collections → FalkorDB graphs → Tantivy indices →
	// Redis keys → DEK destruction. Order matters for crash-
	// recovery: tiers earlier in the list must be safe to re-
	// run idempotently after a restart between this tier and the
	// next.
	Sweepers []TenantSweeper

	// Drain is how long the deleter waits between flipping the
	// status to pending_deletion and starting the sweep, so the
	// pipeline can drain in-flight messages. Defaults to 30s in
	// production; tests pass 0 to skip the sleep.
	Drain time.Duration

	// Audit, when non-nil, receives `tenant.deletion_requested`
	// and `tenant.deleted` events.
	Audit AuditWriter
}

// TenantDeleter orchestrates tenant key destruction. Construction
// validates the config; the unit of work is one Delete call.
type TenantDeleter struct {
	cfg TenantDeleterConfig
}

// NewTenantDeleter validates cfg and returns a deleter. Sweepers
// may be empty for tests that exercise only the state-machine.
func NewTenantDeleter(cfg TenantDeleterConfig) (*TenantDeleter, error) {
	if cfg.Status == nil {
		return nil, errors.New("admin: tenant deleter: nil Status")
	}
	return &TenantDeleter{cfg: cfg}, nil
}

// Delete runs the five-step workflow synchronously. requestedBy is
// the actor (admin user / service account) that initiated the
// destruction; it surfaces in the audit emissions.
//
//  1. Mark tenant pending_deletion (status table + audit).
//  2. Drain the pipeline (sleep cfg.Drain).
//  3. Run every sweeper in order; first error short-circuits and
//     leaves the tenant in pending_deletion for retry.
//  4. (Implicit, last sweeper) destroy DEKs via the credential
//     store sweeper supplied by the caller.
//  5. Mark tenant deleted (status table + audit).
//
// The function returns nil only after step 5 succeeds; callers
// observing a non-nil error MUST retry the same Delete to drive
// the workflow forward.
func (d *TenantDeleter) Delete(ctx context.Context, tenantID, requestedBy string) error {
	if tenantID == "" {
		return errors.New("admin: tenant deleter: empty tenant_id")
	}

	// Step 1: mark pending_deletion.
	if err := d.cfg.Status.UpsertStatus(ctx, tenantID, TenantStatusPendingDeletion); err != nil {
		return fmt.Errorf("admin: mark pending: %w", err)
	}
	d.emitAudit(ctx, tenantID, requestedBy, audit.Action("tenant.deletion_requested"), "requested")

	// Step 2: drain. The pipeline consumer pauses partitions for
	// the tenant once it sees the pending_deletion status, but the
	// fixed sleep gives any already-dispatched message time to
	// either commit or land on the DLQ.
	if d.cfg.Drain > 0 {
		t := time.NewTimer(d.cfg.Drain)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}

	// Steps 3 + 4: storage sweep + DEK destruction.
	for i, sw := range d.cfg.Sweepers {
		if err := sw.ForgetTenant(ctx, tenantID); err != nil {
			return fmt.Errorf("admin: sweeper[%d]: %w", i, err)
		}
	}

	// Step 5: mark deleted.
	if err := d.cfg.Status.UpsertStatus(ctx, tenantID, TenantStatusDeleted); err != nil {
		return fmt.Errorf("admin: mark deleted: %w", err)
	}
	d.emitAudit(ctx, tenantID, requestedBy, audit.Action("tenant.deleted"), "completed")
	return nil
}

func (d *TenantDeleter) emitAudit(ctx context.Context, tenantID, actor string, action audit.Action, phase string) {
	if d.cfg.Audit == nil {
		return
	}
	_ = d.cfg.Audit.Create(ctx, audit.NewAuditLog(
		tenantID,
		actor,
		action,
		"tenant",
		tenantID,
		audit.JSONMap{"phase": phase},
		"",
	))
}

// TenantStatusRepoGORM persists the tenant_status column added in
// migrations/008_tenant_status.sql. It is a thin upsert so the
// admin handler does not need to import gorm directly.
type TenantStatusRepoGORM struct {
	db *gorm.DB
}

// NewTenantStatusRepoGORM constructs a TenantStatusRepoGORM from a
// *gorm.DB. The repo is read/write but tenant scope is enforced at
// the call site (every method takes a tenantID).
func NewTenantStatusRepoGORM(db *gorm.DB) *TenantStatusRepoGORM {
	return &TenantStatusRepoGORM{db: db}
}

// tenantRow mirrors the shape of migrations/008_tenant_status.sql.
type tenantRow struct {
	TenantID     string    `gorm:"primaryKey;column:tenant_id"`
	TenantStatus string    `gorm:"column:tenant_status;not null;default:'active'"`
	CreatedAt    time.Time `gorm:"column:created_at;not null;default:now()"`
	UpdatedAt    time.Time `gorm:"column:updated_at;not null;default:now()"`
}

func (tenantRow) TableName() string { return "tenants" }

// UpsertStatus inserts or updates the tenant's status row. Implemented
// as a primary-key Save which gorm translates to INSERT ... ON
// CONFLICT UPDATE on Postgres and to REPLACE INTO on SQLite (used by
// the unit tests).
func (r *TenantStatusRepoGORM) UpsertStatus(ctx context.Context, tenantID string, status TenantStatus) error {
	if tenantID == "" {
		return errors.New("admin: empty tenant_id")
	}
	row := tenantRow{
		TenantID:     tenantID,
		TenantStatus: string(status),
		UpdatedAt:    time.Now().UTC(),
	}
	return r.db.WithContext(ctx).Save(&row).Error
}

// GetStatus returns the persisted status for tenantID. A missing row
// returns TenantStatusActive (the implicit default for tenants that
// have never had a delete issued).
func (r *TenantStatusRepoGORM) GetStatus(ctx context.Context, tenantID string) (TenantStatus, error) {
	if tenantID == "" {
		return "", errors.New("admin: empty tenant_id")
	}
	var row tenantRow
	err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Limit(1).Find(&row).Error
	if err != nil {
		return "", err
	}
	if row.TenantStatus == "" {
		return TenantStatusActive, nil
	}
	return TenantStatus(row.TenantStatus), nil
}

// TenantDeleteHandler exposes the TenantDeleter through the
// `DELETE /v1/admin/tenants/:tenant_id` endpoint. The handler runs
// the workflow synchronously — production deployments should expect
// the call to take seconds (sweep latency) and surface that in the
// admin UI.
type TenantDeleteHandler struct {
	deleter *TenantDeleter
}

// NewTenantDeleteHandler validates inputs and constructs a handler.
func NewTenantDeleteHandler(deleter *TenantDeleter) (*TenantDeleteHandler, error) {
	if deleter == nil {
		return nil, errors.New("admin: nil deleter")
	}
	return &TenantDeleteHandler{deleter: deleter}, nil
}

// Register mounts the endpoint under rg.
//
//	DELETE /v1/admin/tenants/:tenant_id — full key destruction.
func (h *TenantDeleteHandler) Register(rg *gin.RouterGroup) {
	rg.DELETE("/v1/admin/tenants/:tenant_id", h.delete)
}

func (h *TenantDeleteHandler) delete(c *gin.Context) {
	tenantID := c.Param("tenant_id")
	if tenantID == "" {
		c.JSON(400, gin.H{"error": "missing tenant_id"})
		return
	}
	authVal, ok := c.Get(audit.TenantContextKey)
	if !ok {
		c.JSON(401, gin.H{"error": "missing tenant context"})
		return
	}
	authTenant, _ := authVal.(string)
	// Tenant deletion is a self-service-only operation: admins of
	// tenant A cannot delete tenant B even with the right token.
	if authTenant != tenantID {
		c.JSON(403, gin.H{"error": "tenant mismatch"})
		return
	}
	actor, _ := c.Get(audit.ActorContextKey)
	actorID, _ := actor.(string)
	// Decouple the sweep from the HTTP client's lifetime: a 30 s
	// drain plus per-tier sweep can exceed typical reverse-proxy
	// idle timeouts, and a client disconnect mid-sweep would
	// otherwise leave the tenant stuck in pending_deletion. We
	// keep the request context's values (audit / trace) but drop
	// its cancellation so the workflow can complete or fail on
	// its own merits.
	sweepCtx := context.WithoutCancel(c.Request.Context())
	if err := h.deleter.Delete(sweepCtx, tenantID, actorID); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(202, gin.H{
		"tenant_id":     tenantID,
		"tenant_status": string(TenantStatusDeleted),
	})
}
