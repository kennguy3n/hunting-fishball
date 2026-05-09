package policy

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// LiveResolverGORM resolves a PolicySnapshot for a (tenant, channel)
// pair by reading the live tables LiveStoreGORM writes:
//
//   - tenant_policies     (default privacy mode per tenant)
//   - channel_policies    (per-channel overrides)
//   - policy_acl_rules    (allow/deny rules)
//   - recipient_policies  (skill allow/deny + per-channel default)
//
// It is the read counterpart to LiveStoreGORM.ApplySnapshot. The
// retrieval handler and the Phase 4 simulator's LiveResolver wire to
// the same instance so both observe the same live state.
//
// EffectiveMode follows policy.EffectiveMode (strictest of tenant
// and channel rows). When neither row is present, EffectiveMode
// stays empty so the caller can fall back to its own default
// (HandlerConfig.DefaultPrivacyMode); we do NOT collapse-to-NoAI
// here because doing so would brick brand-new tenants that have
// not yet installed a tenant_policies row.
//
// ACL and recipient rules are scoped to the (tenant, channel)
// tuple. When channelID is empty the resolver returns the tenant-
// wide rule set; when channelID is set we union the tenant-wide
// rules with the channel-specific rules so admins can layer
// per-channel overrides on top of the tenant baseline. Channel-
// specific deny rules win because AllowDenyList.Evaluate already
// short-circuits on any deny match.
type LiveResolverGORM struct {
	db *gorm.DB
}

// NewLiveResolverGORM constructs a LiveResolverGORM from a *gorm.DB.
func NewLiveResolverGORM(db *gorm.DB) *LiveResolverGORM {
	return &LiveResolverGORM{db: db}
}

// Resolve implements PolicyResolver. tenantID is required;
// channelID may be empty for tenant-wide retrievals.
func (r *LiveResolverGORM) Resolve(ctx context.Context, tenantID, channelID string) (PolicySnapshot, error) {
	if tenantID == "" {
		return PolicySnapshot{}, errors.New("policy: Resolve requires tenantID")
	}

	t := r.db.WithContext(ctx)

	tenantMode, err := loadTenantMode(t, tenantID)
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("policy: load tenant mode: %w", err)
	}

	var (
		channelMode      PrivacyMode
		channelRecipDef  string
		hasChannelPolicy bool
	)
	if channelID != "" {
		channelMode, channelRecipDef, hasChannelPolicy, err = loadChannelMode(t, tenantID, channelID)
		if err != nil {
			return PolicySnapshot{}, fmt.Errorf("policy: load channel mode: %w", err)
		}
	}

	effective := resolveEffectiveMode(tenantMode, channelMode, hasChannelPolicy)

	acl, err := loadACLRules(t, tenantID, channelID)
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("policy: load acl rules: %w", err)
	}

	recipient, err := loadRecipientRules(t, tenantID, channelID, channelRecipDef, hasChannelPolicy)
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("policy: load recipient rules: %w", err)
	}

	return PolicySnapshot{
		EffectiveMode: effective,
		ACL:           acl,
		Recipient:     recipient,
	}, nil
}

// loadTenantMode returns the privacy mode for tenantID. A missing
// row returns "" (the caller treats that as "no tenant policy
// installed" and falls back to the channel mode or default).
func loadTenantMode(tx *gorm.DB, tenantID string) (PrivacyMode, error) {
	var row tenantPolicyRow
	err := tx.Where("tenant_id = ?", tenantID).Limit(1).Find(&row).Error
	if err != nil {
		return "", err
	}
	if row.PrivacyMode == "" {
		return "", nil
	}
	return PrivacyMode(row.PrivacyMode), nil
}

// loadChannelMode returns (mode, recipientDefault, present, err)
// for the (tenant, channel) channel_policies row. When no row
// matches, present=false and the (mode, recipientDefault) values
// are zero.
func loadChannelMode(tx *gorm.DB, tenantID, channelID string) (PrivacyMode, string, bool, error) {
	var row channelPolicyRow
	err := tx.Where("tenant_id = ? AND channel_id = ?", tenantID, channelID).Limit(1).Find(&row).Error
	if err != nil {
		return "", "", false, err
	}
	if row.PrivacyMode == "" {
		return "", "", false, nil
	}
	return PrivacyMode(row.PrivacyMode), row.RecipientDefault, true, nil
}

// resolveEffectiveMode picks the strictest of (tenant, channel) and
// degrades safely when one or both rows are absent. The "no rows
// at all" case returns "" so HandlerConfig.DefaultPrivacyMode wins.
func resolveEffectiveMode(tenantMode, channelMode PrivacyMode, hasChannel bool) PrivacyMode {
	switch {
	case tenantMode != "" && hasChannel:
		return EffectiveMode(tenantMode, channelMode)
	case tenantMode != "":
		return tenantMode
	case hasChannel:
		return channelMode
	default:
		return ""
	}
}

// loadACLRules fetches policy_acl_rules for (tenantID, channelID).
// When channelID is empty we return only the tenant-wide rule set
// (rows with NULL or empty channel_id). When channelID is set we
// union the tenant-wide rules with the channel-specific rules so a
// channel inherits the tenant baseline and can layer overrides on
// top — denies always win.
func loadACLRules(tx *gorm.DB, tenantID, channelID string) (*AllowDenyList, error) {
	var rows []aclRuleRow
	q := tx.Where("tenant_id = ?", tenantID)
	if channelID == "" {
		q = q.Where("channel_id IS NULL OR channel_id = ?", "")
	} else {
		q = q.Where("channel_id IS NULL OR channel_id = ? OR channel_id = ?", "", channelID)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := &AllowDenyList{
		TenantID:  tenantID,
		ChannelID: channelID,
		Rules:     make([]ACLRule, 0, len(rows)),
	}
	for _, r := range rows {
		out.Rules = append(out.Rules, ACLRule{
			SourceID:    r.SourceID,
			NamespaceID: r.NamespaceID,
			PathGlob:    r.PathGlob,
			Action:      ACLAction(r.Action),
			ComputeTier: r.ComputeTier,
		})
	}
	return out, nil
}

// loadRecipientRules fetches recipient_policies for (tenantID,
// channelID), seeding RecipientPolicy.DefaultAllow from the channel
// row's recipient_default column (allow → true, deny → false). When
// no channel row exists, default-allow is true.
func loadRecipientRules(tx *gorm.DB, tenantID, channelID, channelRecipDef string, hasChannel bool) (*RecipientPolicy, error) {
	var rows []recipientRuleRow
	q := tx.Where("tenant_id = ?", tenantID)
	if channelID == "" {
		q = q.Where("channel_id = ?", "")
	} else {
		q = q.Where("channel_id = ? OR channel_id = ?", "", channelID)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}

	defaultAllow := !hasChannel || channelRecipDef != "deny"

	if len(rows) == 0 && defaultAllow {
		return nil, nil
	}
	out := &RecipientPolicy{
		TenantID:     tenantID,
		ChannelID:    channelID,
		DefaultAllow: defaultAllow,
		Rules:        make([]RecipientRule, 0, len(rows)),
	}
	for _, r := range rows {
		out.Rules = append(out.Rules, RecipientRule{
			SkillID: r.SkillID,
			Action:  ACLAction(r.Action),
		})
	}
	return out, nil
}
