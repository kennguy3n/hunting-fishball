package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// HealthStatus is the derived health bucket the admin portal renders.
// Computed from last_success_at, lag, and error_count thresholds in
// HealthThresholds.
type HealthStatus string

const (
	// HealthStatusUnknown is the default when no observations exist
	// for the source yet (newly connected, never synced).
	HealthStatusUnknown HealthStatus = "unknown"
	// HealthStatusHealthy means recent successes, low lag, no errors.
	HealthStatusHealthy HealthStatus = "healthy"
	// HealthStatusDegraded means lag or error_count exceeds the warn
	// threshold but the source is still making progress.
	HealthStatusDegraded HealthStatus = "degraded"
	// HealthStatusFailing means error_count or stale-success exceeds
	// the alert threshold; the source needs operator attention.
	HealthStatusFailing HealthStatus = "failing"
)

// Health is the GORM model for the source_health table.
type Health struct {
	TenantID      string       `gorm:"type:char(26);not null;column:tenant_id;primaryKey" json:"tenant_id"`
	SourceID      string       `gorm:"type:char(26);not null;column:source_id;primaryKey" json:"source_id"`
	LastSuccessAt *time.Time   `gorm:"column:last_success_at" json:"last_success_at,omitempty"`
	LastFailureAt *time.Time   `gorm:"column:last_failure_at" json:"last_failure_at,omitempty"`
	Lag           int          `gorm:"not null;default:0;column:lag" json:"lag"`
	ErrorCount    int          `gorm:"not null;default:0;column:error_count" json:"error_count"`
	Status        HealthStatus `gorm:"type:varchar(16);not null;default:'unknown';column:status" json:"status"`
	UpdatedAt     time.Time    `gorm:"not null;default:now();column:updated_at" json:"updated_at"`
}

// TableName overrides the default GORM pluralization.
func (Health) TableName() string { return "source_health" }

// HealthThresholds configures the status derivation. Defaults are
// provided in DefaultThresholds; production wiring overrides them via
// env vars.
type HealthThresholds struct {
	// LagWarn / LagAlert are pending-message thresholds that bump the
	// status to degraded / failing.
	LagWarn  int
	LagAlert int

	// ErrorWarn / ErrorAlert are rolling-window error counts that
	// bump the status to degraded / failing.
	ErrorWarn  int
	ErrorAlert int

	// StaleSuccess marks the source `failing` when the last success
	// is older than this. 0 disables the check.
	StaleSuccess time.Duration
}

// DefaultThresholds is the canonical health-derivation configuration.
// Tuned for the current B2B portal workload; tweak via env in
// cmd/api/main.go.
var DefaultThresholds = HealthThresholds{
	LagWarn:      100,
	LagAlert:     1000,
	ErrorWarn:    5,
	ErrorAlert:   25,
	StaleSuccess: 24 * time.Hour,
}

// DeriveStatus computes a HealthStatus from the row + thresholds. The
// derivation runs on every Update so the persisted Status field is
// always consistent with the underlying counters.
func DeriveStatus(h *Health, thr HealthThresholds, now time.Time) HealthStatus {
	if h == nil {
		return HealthStatusUnknown
	}
	if h.LastSuccessAt == nil && h.LastFailureAt == nil {
		return HealthStatusUnknown
	}
	switch {
	case thr.ErrorAlert > 0 && h.ErrorCount >= thr.ErrorAlert:
		return HealthStatusFailing
	case thr.LagAlert > 0 && h.Lag >= thr.LagAlert:
		return HealthStatusFailing
	case thr.StaleSuccess > 0 && h.LastSuccessAt != nil && now.Sub(*h.LastSuccessAt) > thr.StaleSuccess:
		return HealthStatusFailing
	case thr.ErrorWarn > 0 && h.ErrorCount >= thr.ErrorWarn:
		return HealthStatusDegraded
	case thr.LagWarn > 0 && h.Lag >= thr.LagWarn:
		return HealthStatusDegraded
	default:
		return HealthStatusHealthy
	}
}

// HealthRepository is the read/write port for source_health.
type HealthRepository struct {
	db  *gorm.DB
	thr HealthThresholds
}

// NewHealthRepository wires a HealthRepository.
func NewHealthRepository(db *gorm.DB, thr HealthThresholds) *HealthRepository {
	if thr == (HealthThresholds{}) {
		thr = DefaultThresholds
	}
	return &HealthRepository{db: db, thr: thr}
}

// Get returns the health row for (tenantID, sourceID), or nil if the
// row does not yet exist.
func (r *HealthRepository) Get(ctx context.Context, tenantID, sourceID string) (*Health, error) {
	if tenantID == "" || sourceID == "" {
		return nil, errors.New("admin: missing tenant/source")
	}
	var h Health
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		First(&h).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("admin: get health: %w", err)
	}
	return &h, nil
}

// RecordSuccess increments success counters and (re)derives the
// status. Creates the row on first observation.
func (r *HealthRepository) RecordSuccess(ctx context.Context, tenantID, sourceID string) error {
	return r.upsert(ctx, tenantID, sourceID, func(h *Health, now time.Time) {
		h.LastSuccessAt = &now
		h.ErrorCount = 0 // success resets the rolling failure window
	})
}

// RecordFailure increments error_count and (re)derives the status.
func (r *HealthRepository) RecordFailure(ctx context.Context, tenantID, sourceID string) error {
	return r.upsert(ctx, tenantID, sourceID, func(h *Health, now time.Time) {
		h.LastFailureAt = &now
		h.ErrorCount++
	})
}

// SetLag overrides the lag counter (the consumer computes lag from
// Kafka offsets and pushes it here).
func (r *HealthRepository) SetLag(ctx context.Context, tenantID, sourceID string, lag int) error {
	if lag < 0 {
		lag = 0
	}
	return r.upsert(ctx, tenantID, sourceID, func(h *Health, _ time.Time) {
		h.Lag = lag
	})
}

func (r *HealthRepository) upsert(ctx context.Context, tenantID, sourceID string, mutate func(*Health, time.Time)) error {
	if tenantID == "" || sourceID == "" {
		return errors.New("admin: missing tenant/source")
	}
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var h Health
		err := tx.Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).First(&h).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			h = Health{TenantID: tenantID, SourceID: sourceID, Status: HealthStatusUnknown}
		} else if err != nil {
			return fmt.Errorf("admin: load health: %w", err)
		}
		mutate(&h, now)
		h.UpdatedAt = now
		h.Status = DeriveStatus(&h, r.thr, now)
		// Use Save which performs an INSERT ... ON CONFLICT-equivalent
		// upsert for primary-key composite tables.
		if err := tx.Save(&h).Error; err != nil {
			return fmt.Errorf("admin: save health: %w", err)
		}
		return nil
	})
}
