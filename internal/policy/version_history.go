// version_history.go — Round-5 Task 15.
//
// PolicyVersionRepository persists every promote/reject of a draft
// as an immutable row in policy_versions so admins can audit the
// lineage of a tenant's policy and rebuild a draft from any past
// snapshot via the rollback handler.
//
// The schema mirrors migrations/017_policy_versions.sql exactly —
// snapshot is JSONB in production Postgres, TEXT in the SQLite
// test fakes; DraftPayload's driver.Valuer/sql.Scanner round-trip
// gives us the same JSON-shape on both backends.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// PolicyVersion is a single promote/reject of a Draft. The
// snapshot column captures the entire DraftPayload — same shape
// as Draft.Payload — so a rollback reconstructs a new Draft
// without needing the draft row to still exist.
type PolicyVersion struct {
	ID        string       `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID  string       `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	ChannelID string       `gorm:"type:varchar(128);not null;column:channel_id" json:"channel_id"`
	DraftID   string       `gorm:"type:char(26);not null;column:draft_id" json:"draft_id"`
	Action    string       `gorm:"type:varchar(32);not null;column:action" json:"action"`
	ActorID   string       `gorm:"type:varchar(64);not null;column:actor_id" json:"actor_id"`
	Snapshot  DraftPayload `gorm:"type:jsonb;not null;column:snapshot" json:"snapshot"`
	CreatedAt time.Time    `gorm:"type:timestamptz;not null;default:now();column:created_at" json:"created_at"`
}

// TableName overrides GORM's default plural-snake naming so the
// row maps onto policy_versions exactly.
func (PolicyVersion) TableName() string { return "policy_versions" }

// PolicyVersionRepository is the GORM-backed accessor for
// policy_versions. Tests inject the same SQLite handle used by
// DraftRepository so version writes participate in the same
// transactions as draft state flips.
type PolicyVersionRepository struct {
	db *gorm.DB
}

// NewPolicyVersionRepository wraps a *gorm.DB so the same handle
// can be used for transactional outbox writes.
func NewPolicyVersionRepository(db *gorm.DB) *PolicyVersionRepository {
	return &PolicyVersionRepository{db: db}
}

// DB exposes the underlying *gorm.DB for callers that need to
// open their own transaction (e.g. the rollback handler).
func (r *PolicyVersionRepository) DB() *gorm.DB { return r.db }

// Insert appends a row. The supplied tx — when non-nil — is the
// outer transaction so the version row commits atomically with
// the promote/reject business write.
func (r *PolicyVersionRepository) Insert(ctx context.Context, tx *gorm.DB, v *PolicyVersion) error {
	if v == nil {
		return errors.New("policy: PolicyVersionRepository.Insert nil row")
	}
	if v.ID == "" {
		v.ID = ulid.Make().String()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	db := r.db
	if tx != nil {
		db = tx
	}
	return db.WithContext(ctx).Create(v).Error
}

// VersionListFilter narrows a List query.
type VersionListFilter struct {
	TenantID  string
	ChannelID string // optional
	Limit     int    // 0 → 50; capped at 200
	Cursor    string // ULID; rows with id < Cursor are returned
}

// List returns versions for a tenant in descending id order so
// callers see the newest promotion first. The 200-cap mirrors the
// admin pagination convention from Task 3.
func (r *PolicyVersionRepository) List(ctx context.Context, f VersionListFilter) ([]PolicyVersion, error) {
	if f.TenantID == "" {
		return nil, errors.New("policy: VersionListFilter requires TenantID")
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	// The cap accommodates the handler's fetch-N+1 pagination
	// pattern: callers ask for `userLimit+1` so they can detect
	// whether more pages exist without an extra COUNT(*). The
	// admin pagination ceiling is 200, so the repo cap is 201 to
	// pass that extra row through. See policy_history_handler.go
	// list() for the consumer.
	if f.Limit > 201 {
		f.Limit = 201
	}
	q := r.db.WithContext(ctx).
		Where("tenant_id = ?", f.TenantID).
		Order("id DESC").
		Limit(f.Limit)
	if f.ChannelID != "" {
		q = q.Where("channel_id = ?", f.ChannelID)
	}
	if f.Cursor != "" {
		q = q.Where("id < ?", f.Cursor)
	}
	var rows []PolicyVersion
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Get returns a single version by (tenant, id) — tenant scoping is
// non-negotiable so a malicious caller can't reference another
// tenant's version-id and steal its snapshot.
func (r *PolicyVersionRepository) Get(ctx context.Context, tenantID, id string) (*PolicyVersion, error) {
	if tenantID == "" {
		return nil, errors.New("policy: PolicyVersionRepository.Get requires tenantID")
	}
	if id == "" {
		return nil, errors.New("policy: PolicyVersionRepository.Get requires id")
	}
	var v PolicyVersion
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&v).Error
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// snapshotJSON returns the version's snapshot as raw JSON for
// callers that want to project just the policy data without
// re-marshalling the entire row.
func (v *PolicyVersion) SnapshotJSON() ([]byte, error) {
	return json.Marshal(v.Snapshot)
}
