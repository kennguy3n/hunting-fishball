package policy

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DraftStatus is the lifecycle marker on a policy_drafts row. The
// promotion workflow is the only path that flips draft → promoted;
// the reject workflow is the only path that flips draft → rejected.
type DraftStatus string

const (
	// DraftStatusDraft is the only mutable state. Drafts in this
	// state can be edited, simulated, promoted, or rejected.
	DraftStatusDraft DraftStatus = "draft"

	// DraftStatusPromoted is terminal — the draft's PolicySnapshot
	// has been applied to the live policy tables and the row is
	// preserved for audit.
	DraftStatusPromoted DraftStatus = "promoted"

	// DraftStatusRejected is terminal — the draft was explicitly
	// rejected and the row is preserved for audit.
	DraftStatusRejected DraftStatus = "rejected"
)

// ErrDraftNotFound is returned by Get / Promote / Reject when no row
// matches the (tenant_id, id) tuple. Cross-tenant lookups also
// surface this error so callers cannot distinguish tenant leakage
// from a genuine miss.
var ErrDraftNotFound = errors.New("policy: draft not found")

// ErrDraftTerminal is returned by Promote / Reject when the row
// exists but is already in a terminal status (promoted/rejected).
// Handlers translate this to HTTP 409.
var ErrDraftTerminal = errors.New("policy: draft is in a terminal status")

// Draft is the GORM model for the policy_drafts table. The schema
// is defined in migrations/005_policy_drafts.sql.
//
// Drafts are isolated from live: the retrieval handler's
// PolicyResolver only reads from tenant_policies / channel_policies
// / policy_acl_rules / recipient_policies. The Payload column
// stores the entire proposed PolicySnapshot so the simulator can
// what-if it without joining across the live tables.
type Draft struct {
	ID        string `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID  string `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	ChannelID string `gorm:"type:char(26);column:channel_id" json:"channel_id,omitempty"`

	// Payload is the proposed PolicySnapshot serialised as JSON.
	// Stored as JSONB in production Postgres; SQLite tests fall
	// back to TEXT but the round-trip semantics match.
	Payload DraftPayload `gorm:"type:jsonb;not null;default:'{}';column:payload" json:"payload"`

	Status      DraftStatus `gorm:"type:varchar(16);not null;default:'draft';column:status" json:"status"`
	CreatedBy   string      `gorm:"column:created_by" json:"created_by,omitempty"`
	CreatedAt   time.Time   `gorm:"not null;default:now();column:created_at" json:"created_at"`
	PromotedAt  *time.Time  `gorm:"column:promoted_at" json:"promoted_at,omitempty"`
	PromotedBy  string      `gorm:"column:promoted_by" json:"promoted_by,omitempty"`
	RejectReason string     `gorm:"column:reject_reason" json:"reject_reason,omitempty"`
}

// TableName overrides the default GORM pluralisation.
func (Draft) TableName() string { return "policy_drafts" }

// DraftPayload is the JSON-serialised PolicySnapshot. Implements
// driver.Valuer / sql.Scanner for direct GORM round-tripping.
type DraftPayload struct {
	Snapshot PolicySnapshot `json:"snapshot"`
}

// MarshalJSON marshals the inner snapshot directly so the JSONB
// column round-trips as `{"snapshot":...}`.
func (p DraftPayload) MarshalJSON() ([]byte, error) {
	type wire struct {
		Snapshot PolicySnapshot `json:"snapshot"`
	}
	return json.Marshal(wire{Snapshot: p.Snapshot})
}

// UnmarshalJSON unmarshals the inner snapshot.
func (p *DraftPayload) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*p = DraftPayload{}
		return nil
	}
	type wire struct {
		Snapshot PolicySnapshot `json:"snapshot"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	p.Snapshot = w.Snapshot
	return nil
}

// Value implements driver.Valuer.
func (p DraftPayload) Value() (driver.Value, error) {
	return p.MarshalJSON()
}

// Scan implements sql.Scanner. Tolerates []byte and string
// representations so SQLite (text) and Postgres (jsonb) both work.
func (p *DraftPayload) Scan(src any) error {
	if src == nil {
		*p = DraftPayload{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("policy: DraftPayload.Scan: unsupported source type")
	}
	if len(b) == 0 {
		*p = DraftPayload{}
		return nil
	}
	return p.UnmarshalJSON(b)
}

// NewDraft constructs a Draft with a fresh ULID, status=draft, and
// the supplied snapshot. Callers may override CreatedBy after
// construction; the repository's Create method validates the row.
func NewDraft(tenantID, channelID, createdBy string, snap PolicySnapshot) *Draft {
	return &Draft{
		ID:        ulid.Make().String(),
		TenantID:  tenantID,
		ChannelID: channelID,
		Payload:   DraftPayload{Snapshot: snap.Clone()},
		Status:    DraftStatusDraft,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
	}
}

// Validate checks the invariants the table column constraints would
// otherwise reject at insert time.
func (d *Draft) Validate() error {
	if d.ID == "" {
		return errors.New("policy: missing draft ID")
	}
	if d.TenantID == "" {
		return errors.New("policy: missing draft tenant_id")
	}
	switch d.Status {
	case DraftStatusDraft, DraftStatusPromoted, DraftStatusRejected:
	default:
		return errors.New("policy: invalid draft status")
	}
	return nil
}

// DraftRepository is the read/write port for the policy_drafts
// table. All methods are tenant-scoped: a misconfigured caller
// cannot pull another tenant's row.
type DraftRepository struct {
	db *gorm.DB
}

// NewDraftRepository wires a DraftRepository to the supplied
// *gorm.DB.
func NewDraftRepository(db *gorm.DB) *DraftRepository {
	return &DraftRepository{db: db}
}

// DB exposes the underlying handle so the promotion workflow can
// run audit + live-policy writes in the same transaction as the
// draft state flip.
func (r *DraftRepository) DB() *gorm.DB { return r.db }

// Create inserts a new draft row.
func (r *DraftRepository) Create(ctx context.Context, d *Draft) error {
	if d == nil {
		return errors.New("policy: nil draft")
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Create(d).Error; err != nil {
		return fmt.Errorf("policy: insert draft: %w", err)
	}
	return nil
}

// Get returns a single draft, scoped to tenantID. Cross-tenant
// lookups return ErrDraftNotFound — never the row.
func (r *DraftRepository) Get(ctx context.Context, tenantID, id string) (*Draft, error) {
	if tenantID == "" || id == "" {
		return nil, ErrDraftNotFound
	}
	var d Draft
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("policy: get draft: %w", err)
	}
	return &d, nil
}

// DraftListFilter narrows ListDrafts. TenantID is mandatory.
type DraftListFilter struct {
	TenantID string
	Status   DraftStatus
	PageSize int
}

// List returns drafts for tenantID, ordered by id DESC (ULIDs
// embed timestamps so id-DESC == newest-first).
func (r *DraftRepository) List(ctx context.Context, f DraftListFilter) ([]Draft, error) {
	if f.TenantID == "" {
		return nil, errors.New("policy: DraftListFilter.TenantID is required")
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
	var out []Draft
	if err := q.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("policy: list drafts: %w", err)
	}
	return out, nil
}

// MarkPromoted flips a draft into the promoted terminal status. The
// caller (the promotion workflow) is responsible for applying the
// snapshot to live tables in the same transaction. Returns
// ErrDraftNotFound if the row does not exist for the tenant and
// ErrDraftTerminal if the row is already in a terminal status.
func (r *DraftRepository) MarkPromoted(ctx context.Context, tx *gorm.DB, tenantID, id, actorID string) (*Draft, error) {
	if tx == nil {
		tx = r.db
	}
	return r.transition(ctx, tx, tenantID, id, transitionMarkPromoted{actor: actorID})
}

// MarkRejected flips a draft into the rejected terminal status with
// an optional human-readable reason.
func (r *DraftRepository) MarkRejected(ctx context.Context, tx *gorm.DB, tenantID, id, actorID, reason string) (*Draft, error) {
	if tx == nil {
		tx = r.db
	}
	return r.transition(ctx, tx, tenantID, id, transitionMarkRejected{actor: actorID, reason: reason})
}

// transitionApply describes a draft state flip. Implementations are
// the only place that mutate Draft.Status — having the switch in
// one method keeps the (status, side-effects) pairing correct.
type transitionApply interface {
	apply(d *Draft)
}

type transitionMarkPromoted struct{ actor string }

func (t transitionMarkPromoted) apply(d *Draft) {
	now := time.Now().UTC()
	d.Status = DraftStatusPromoted
	d.PromotedAt = &now
	d.PromotedBy = t.actor
}

type transitionMarkRejected struct{ actor, reason string }

func (t transitionMarkRejected) apply(d *Draft) {
	now := time.Now().UTC()
	d.Status = DraftStatusRejected
	// Re-use PromotedAt / PromotedBy as the "decided at / by"
	// audit columns; the table comment documents this. Keeps the
	// schema small without a parallel rejected_at / rejected_by
	// pair that would mostly stay NULL.
	d.PromotedAt = &now
	d.PromotedBy = t.actor
	d.RejectReason = t.reason
}

func (r *DraftRepository) transition(ctx context.Context, tx *gorm.DB, tenantID, id string, t transitionApply) (*Draft, error) {
	if tenantID == "" || id == "" {
		return nil, ErrDraftNotFound
	}
	// SELECT … FOR UPDATE inside the caller's transaction
	// serialises concurrent Promote/Reject calls on the same row.
	// Without it two callers could both observe status='draft',
	// both pass the guard below, and both run apply()+Save —
	// double-promoting the snapshot and emitting duplicate
	// audit events. SQLite (used in tests) tolerates the locking
	// clause as a no-op; Postgres acquires a row lock until commit.
	var d Draft
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("policy: load draft: %w", err)
	}
	if d.Status != DraftStatusDraft {
		return nil, ErrDraftTerminal
	}
	t.apply(&d)
	if err := tx.WithContext(ctx).Save(&d).Error; err != nil {
		return nil, fmt.Errorf("policy: save draft: %w", err)
	}
	return &d, nil
}
