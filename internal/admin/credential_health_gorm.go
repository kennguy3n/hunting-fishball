package admin

// credential_health_gorm.go — Round-8 Task 11.
//
// Postgres-backed CredentialHealth store. The migration that
// introduces the underlying columns is
// migrations/026_credential_valid.sql (ALTERs source_health to add
// credential_valid / credential_checked_at / credential_error).
//
// This file deliberately defines a second GORM model
// (credentialHealthRow) bound to the same source_health table. The
// existing Health struct above doesn't carry the credential
// columns, and adding them there would have rippled through every
// HealthRepository call site. A targeted model keeps the surface
// minimal.

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// credentialHealthRow is the GORM view of the source_health row
// that holds the credential-validation outcome.
type credentialHealthRow struct {
	TenantID            string    `gorm:"column:tenant_id;type:char(26);primaryKey"`
	SourceID            string    `gorm:"column:source_id;type:char(26);primaryKey"`
	CredentialValid     bool      `gorm:"column:credential_valid;not null;default:true"`
	CredentialCheckedAt time.Time `gorm:"column:credential_checked_at"`
	CredentialError     string    `gorm:"column:credential_error;type:text;not null;default:''"`
}

// TableName binds the model to source_health.
func (credentialHealthRow) TableName() string { return "source_health" }

// CredentialHealthGORM is the Postgres-backed implementation.
type CredentialHealthGORM struct{ db *gorm.DB }

// NewCredentialHealthGORM validates inputs.
func NewCredentialHealthGORM(db *gorm.DB) (*CredentialHealthGORM, error) {
	if db == nil {
		return nil, errors.New("credential_health: nil db")
	}
	return &CredentialHealthGORM{db: db}, nil
}

// RecordCredentialCheck persists the result of a single
// connector.Validate() invocation. The upsert sets just the
// credential_* columns and leaves the sync-health counters alone.
func (s *CredentialHealthGORM) RecordCredentialCheck(ctx context.Context, tenantID, sourceID string, valid bool, message string) error {
	if tenantID == "" || sourceID == "" {
		return errors.New("credential_health: missing tenant/source")
	}
	row := credentialHealthRow{
		TenantID: tenantID, SourceID: sourceID,
		CredentialValid: valid, CredentialError: message,
		CredentialCheckedAt: time.Now().UTC(),
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "source_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"credential_valid", "credential_checked_at", "credential_error",
		}),
	}).Create(&row).Error
}

// Get returns the projected credential health row.
func (s *CredentialHealthGORM) Get(ctx context.Context, tenantID, sourceID string) (CredentialCheckRow, bool) {
	if tenantID == "" || sourceID == "" {
		return CredentialCheckRow{}, false
	}
	var r credentialHealthRow
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		First(&r).Error
	if err != nil {
		return CredentialCheckRow{}, false
	}
	if r.CredentialCheckedAt.IsZero() {
		// No credential check has been recorded yet for this row.
		return CredentialCheckRow{}, false
	}
	return CredentialCheckRow{
		TenantID: r.TenantID, SourceID: r.SourceID,
		Valid: r.CredentialValid, Message: r.CredentialError,
		CheckedAt: r.CredentialCheckedAt,
	}, true
}
