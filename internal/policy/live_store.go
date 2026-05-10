package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// LiveStoreGORM applies promoted PolicySnapshots to the live policy
// tables defined in migrations/004_policy.sql:
//
//   - tenant_policies     (default privacy mode per tenant)
//   - channel_policies    (per-channel overrides)
//   - policy_acl_rules    (allow/deny rules)
//   - recipient_policies  (skill allow/deny)
//
// All writes happen inside the transaction supplied by the
// promotion workflow so the live state only flips after the audit
// row + the draft state flip commit.
//
// Snapshot semantics: a tenant-wide draft (channelID == "") replaces
// the tenant's default privacy mode AND the tenant-wide ACL /
// recipient rule set; a channel-scoped draft replaces just the
// channel override + that channel's ACL / recipient rules. We do
// NOT diff-and-merge — a draft is the desired-state representation,
// so promote = "make live look exactly like this".
type LiveStoreGORM struct {
	db *gorm.DB
}

// NewLiveStoreGORM constructs a LiveStoreGORM from a *gorm.DB. The
// handle is the read default; ApplySnapshot accepts a separate tx.
func NewLiveStoreGORM(db *gorm.DB) *LiveStoreGORM {
	return &LiveStoreGORM{db: db}
}

// ApplySnapshot writes snap to the live tables in tx. The contract
// matches policy.LiveStore — see promotion.go.
func (s *LiveStoreGORM) ApplySnapshot(ctx context.Context, tx *gorm.DB, tenantID, channelID string, snap PolicySnapshot) error {
	if tx == nil {
		tx = s.db
	}
	if tenantID == "" {
		return errors.New("policy: ApplySnapshot requires tenantID")
	}

	now := time.Now().UTC()
	t := tx.WithContext(ctx)

	if channelID == "" {
		if err := upsertTenantPolicy(t, tenantID, string(snap.EffectiveMode), now); err != nil {
			return fmt.Errorf("policy: upsert tenant_policies: %w", err)
		}
	} else {
		if err := upsertChannelPolicy(t, tenantID, channelID, string(snap.EffectiveMode), recipientDefault(snap.Recipient), snap.DenyLocalRetrieval, now); err != nil {
			return fmt.Errorf("policy: upsert channel_policies: %w", err)
		}
	}

	if err := replaceACLRules(t, tenantID, channelID, snap.ACL, now); err != nil {
		return fmt.Errorf("policy: replace policy_acl_rules: %w", err)
	}
	if err := replaceRecipientRules(t, tenantID, channelID, snap.Recipient, now); err != nil {
		return fmt.Errorf("policy: replace recipient_policies: %w", err)
	}
	return nil
}

func upsertTenantPolicy(tx *gorm.DB, tenantID, mode string, now time.Time) error {
	row := tenantPolicyRow{
		TenantID:    tenantID,
		PrivacyMode: mode,
		UpdatedAt:   now,
	}
	// We use ON CONFLICT semantics implicitly via Save, which
	// emits an UPDATE when the primary key matches and an INSERT
	// otherwise.
	return tx.Save(&row).Error
}

func upsertChannelPolicy(tx *gorm.DB, tenantID, channelID, mode, recipientDefault string, denyLocal bool, now time.Time) error {
	row := channelPolicyRow{
		TenantID:           tenantID,
		ChannelID:          channelID,
		PrivacyMode:        mode,
		RecipientDefault:   recipientDefault,
		DenyLocalRetrieval: denyLocal,
		UpdatedAt:          now,
	}
	return tx.Save(&row).Error
}

// replaceACLRules wipes the (tenant, channel) ACL rule set and
// re-inserts the snapshot's rule list. ACL rules are unique by
// (tenant_id, channel_id, source_id, namespace_id, path_glob,
// action) but the migration does NOT enforce that constraint, so a
// blind re-insert would accumulate dupes on every promotion. Wipe-
// and-replace is the simplest correct semantic.
func replaceACLRules(tx *gorm.DB, tenantID, channelID string, list *AllowDenyList, now time.Time) error {
	q := tx.Where("tenant_id = ?", tenantID)
	if channelID == "" {
		q = q.Where("channel_id IS NULL OR channel_id = ?", "")
	} else {
		q = q.Where("channel_id = ?", channelID)
	}
	if err := q.Delete(&aclRuleRow{}).Error; err != nil {
		return err
	}
	if list == nil {
		return nil
	}
	if len(list.Rules) == 0 {
		return nil
	}
	rows := make([]aclRuleRow, 0, len(list.Rules))
	for _, r := range list.Rules {
		row := aclRuleRow{
			ID:          ulid.Make().String(),
			TenantID:    tenantID,
			ChannelID:   channelID,
			SourceID:    r.SourceID,
			NamespaceID: r.NamespaceID,
			PathGlob:    r.PathGlob,
			Action:      string(r.Action),
			ComputeTier: r.ComputeTier,
			CreatedAt:   now,
		}
		rows = append(rows, row)
	}
	return tx.Create(&rows).Error
}

// replaceRecipientRules mirrors replaceACLRules for the recipient
// table.
func replaceRecipientRules(tx *gorm.DB, tenantID, channelID string, list *RecipientPolicy, now time.Time) error {
	q := tx.Where("tenant_id = ?", tenantID)
	if channelID == "" {
		q = q.Where("channel_id IS NULL OR channel_id = ?", "")
	} else {
		q = q.Where("channel_id = ?", channelID)
	}
	if err := q.Delete(&recipientRuleRow{}).Error; err != nil {
		return err
	}
	if list == nil || len(list.Rules) == 0 {
		return nil
	}
	rows := make([]recipientRuleRow, 0, len(list.Rules))
	for _, r := range list.Rules {
		row := recipientRuleRow{
			ID:        ulid.Make().String(),
			TenantID:  tenantID,
			ChannelID: channelID,
			SkillID:   r.SkillID,
			Action:    string(r.Action),
			CreatedAt: now,
		}
		rows = append(rows, row)
	}
	return tx.Create(&rows).Error
}

func recipientDefault(r *RecipientPolicy) string {
	if r == nil || r.DefaultAllow {
		return "allow"
	}
	return "deny"
}

// tenantPolicyRow is the GORM model for tenant_policies.
type tenantPolicyRow struct {
	TenantID    string    `gorm:"type:char(26);primaryKey;column:tenant_id"`
	PrivacyMode string    `gorm:"type:varchar(32);not null;column:privacy_mode"`
	CreatedAt   time.Time `gorm:"not null;default:now();column:created_at"`
	UpdatedAt   time.Time `gorm:"not null;default:now();column:updated_at"`
}

func (tenantPolicyRow) TableName() string { return "tenant_policies" }

// channelPolicyRow is the GORM model for channel_policies.
type channelPolicyRow struct {
	TenantID           string    `gorm:"type:char(26);primaryKey;column:tenant_id"`
	ChannelID          string    `gorm:"type:char(26);primaryKey;column:channel_id"`
	PrivacyMode        string    `gorm:"type:varchar(32);not null;column:privacy_mode"`
	RecipientDefault   string    `gorm:"type:varchar(8);not null;default:'allow';column:recipient_default"`
	DenyLocalRetrieval bool      `gorm:"not null;default:false;column:deny_local_retrieval"`
	CreatedAt          time.Time `gorm:"not null;default:now();column:created_at"`
	UpdatedAt          time.Time `gorm:"not null;default:now();column:updated_at"`
}

func (channelPolicyRow) TableName() string { return "channel_policies" }

// AfterFind trims trailing CHAR-padding spaces. Defensive parity with
// migrations/011_varchar_ids.sql so reads from any pre-migration
// snapshot still produce well-formed scope keys.
func (r *channelPolicyRow) AfterFind(_ *gorm.DB) error {
	r.ChannelID = strings.TrimRight(r.ChannelID, " ")
	return nil
}

// aclRuleRow is the GORM model for policy_acl_rules.
type aclRuleRow struct {
	ID          string    `gorm:"type:char(26);primaryKey;column:id"`
	TenantID    string    `gorm:"type:char(26);not null;column:tenant_id"`
	ChannelID   string    `gorm:"type:char(26);column:channel_id"`
	SourceID    string    `gorm:"type:char(26);column:source_id"`
	NamespaceID string    `gorm:"type:varchar(128);column:namespace_id"`
	PathGlob    string    `gorm:"type:varchar(512);column:path_glob"`
	Action      string    `gorm:"type:varchar(8);not null;column:action"`
	ComputeTier string    `gorm:"type:varchar(32);column:compute_tier"`
	CreatedAt   time.Time `gorm:"not null;default:now();column:created_at"`
}

func (aclRuleRow) TableName() string { return "policy_acl_rules" }

// AfterFind trims CHAR-padding spaces from the wildcard sentinels so
// `r.SourceID == ""`, `r.ChannelID == ""`, and the namespace check in
// ruleMatches all behave the same way they do for SQLite/in-memory
// fixtures. Without this, Postgres-backed reads carried 26 spaces in
// the ID columns and the ACL rule never matched the chunk attrs.
func (r *aclRuleRow) AfterFind(_ *gorm.DB) error {
	r.ChannelID = strings.TrimRight(r.ChannelID, " ")
	r.SourceID = strings.TrimRight(r.SourceID, " ")
	return nil
}

// recipientRuleRow is the GORM model for recipient_policies.
type recipientRuleRow struct {
	ID        string    `gorm:"type:char(26);primaryKey;column:id"`
	TenantID  string    `gorm:"type:char(26);not null;column:tenant_id"`
	ChannelID string    `gorm:"type:char(26);not null;column:channel_id"`
	SkillID   string    `gorm:"type:varchar(64);column:skill_id"`
	Action    string    `gorm:"type:varchar(8);not null;column:action"`
	CreatedAt time.Time `gorm:"not null;default:now();column:created_at"`
}

func (recipientRuleRow) TableName() string { return "recipient_policies" }

// AfterFind trims CHAR-padding spaces. See aclRuleRow.AfterFind.
func (r *recipientRuleRow) AfterFind(_ *gorm.DB) error {
	r.ChannelID = strings.TrimRight(r.ChannelID, " ")
	return nil
}
