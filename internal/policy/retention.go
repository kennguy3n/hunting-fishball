// retention.go — Phase 8 / Task 6 retention policy primitives.
//
// PROPOSAL.md §5 specifies that documents inherit the source's
// retention rules and tenants may layer additional policy on top.
// The struct + helpers in this file model that layering: a Resolver
// returns the *effective* MaxAgeDays for a chunk by picking the most
// restrictive non-zero value across (namespace, source, tenant)
// scopes.
package policy

import (
	"errors"
	"fmt"
	"time"
)

// RetentionScope is the layer at which a retention rule applies. The
// fewer the chunks a scope addresses, the more specific the scope.
type RetentionScope string

const (
	RetentionScopeTenant    RetentionScope = "tenant"
	RetentionScopeSource    RetentionScope = "source"
	RetentionScopeNamespace RetentionScope = "namespace"
)

// Validate ensures the scope is one of the documented values.
func (s RetentionScope) Validate() error {
	switch s {
	case RetentionScopeTenant, RetentionScopeSource, RetentionScopeNamespace:
		return nil
	default:
		return fmt.Errorf("retention: invalid scope %q", s)
	}
}

// RetentionPolicy is a single retention rule. ScopeValue is empty
// for tenant-scoped rules and contains the source_id or
// "<source_id>/<namespace_id>" for narrower scopes.
type RetentionPolicy struct {
	ID          string         `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID    string         `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	Scope       RetentionScope `gorm:"type:varchar(16);not null;column:scope" json:"scope"`
	ScopeValue  string         `gorm:"type:varchar(256);not null;default:'';column:scope_value" json:"scope_value,omitempty"`
	MaxAgeDays  int            `gorm:"not null;column:max_age_days" json:"max_age_days"`
	CreatedAt   time.Time      `gorm:"not null;column:created_at" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"not null;column:updated_at" json:"updated_at"`
}

// TableName overrides the GORM default pluralisation.
func (RetentionPolicy) TableName() string { return "retention_policies" }

// Validate enforces invariants that the SQL CHECK constraints don't
// catch (e.g., MaxAgeDays must be non-negative).
func (p *RetentionPolicy) Validate() error {
	if p == nil {
		return errors.New("retention: nil policy")
	}
	if p.TenantID == "" {
		return errors.New("retention: missing tenant_id")
	}
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	if p.Scope != RetentionScopeTenant && p.ScopeValue == "" {
		return fmt.Errorf("retention: scope_value required for scope %q", p.Scope)
	}
	if p.MaxAgeDays < 0 {
		return errors.New("retention: max_age_days must be >= 0")
	}
	return nil
}

// IsExpired returns true when ingestedAt is more than MaxAgeDays in
// the past. A zero MaxAgeDays disables the rule.
func (p RetentionPolicy) IsExpired(ingestedAt time.Time) bool {
	if p.MaxAgeDays <= 0 {
		return false
	}
	if ingestedAt.IsZero() {
		return false
	}
	return time.Since(ingestedAt) > time.Duration(p.MaxAgeDays)*24*time.Hour
}

// ChunkScope describes the address of a single chunk relative to
// the retention layers. Used by Effective() to pick the right rule.
type ChunkScope struct {
	TenantID    string
	SourceID    string
	NamespaceID string
}

// Effective returns the most restrictive non-zero MaxAgeDays across
// the supplied policy set for the chunk addressed by scope. Empty
// rules (MaxAgeDays == 0) are ignored.
//
// Returns ok=false when no rule applies (the worker treats this as
// "do not delete"). When ok is true, the returned policy is a copy
// from the input set so the caller can't mutate the source list.
func Effective(scope ChunkScope, policies []RetentionPolicy) (RetentionPolicy, bool) {
	var best *RetentionPolicy
	for i := range policies {
		p := policies[i]
		if p.TenantID != scope.TenantID || p.MaxAgeDays <= 0 {
			continue
		}
		if !appliesTo(p, scope) {
			continue
		}
		if best == nil || p.MaxAgeDays < best.MaxAgeDays {
			cp := p
			best = &cp
		}
	}
	if best == nil {
		return RetentionPolicy{}, false
	}
	return *best, true
}

func appliesTo(p RetentionPolicy, scope ChunkScope) bool {
	switch p.Scope {
	case RetentionScopeTenant:
		return true
	case RetentionScopeSource:
		return p.ScopeValue == scope.SourceID
	case RetentionScopeNamespace:
		return p.ScopeValue == scope.SourceID+"/"+scope.NamespaceID
	default:
		return false
	}
}

// IsExpired returns true when ingestedAt is older than the effective
// MaxAgeDays for scope. Returns false when no policy applies.
func IsExpired(scope ChunkScope, ingestedAt time.Time, policies []RetentionPolicy) bool {
	p, ok := Effective(scope, policies)
	if !ok {
		return false
	}
	return p.IsExpired(ingestedAt)
}
