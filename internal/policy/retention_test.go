package policy_test

import (
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestRetentionScope_Validate(t *testing.T) {
	t.Parallel()
	for _, s := range []policy.RetentionScope{policy.RetentionScopeTenant, policy.RetentionScopeSource, policy.RetentionScopeNamespace} {
		if err := s.Validate(); err != nil {
			t.Fatalf("scope %q rejected: %v", s, err)
		}
	}
	if err := policy.RetentionScope("bogus").Validate(); err == nil {
		t.Fatalf("expected error for invalid scope")
	}
}

func TestRetentionPolicy_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    policy.RetentionPolicy
		err  bool
	}{
		{"missing tenant", policy.RetentionPolicy{Scope: policy.RetentionScopeTenant, MaxAgeDays: 1}, true},
		{"bad scope", policy.RetentionPolicy{TenantID: "t", Scope: "bad", MaxAgeDays: 1}, true},
		{"missing scope_value for source", policy.RetentionPolicy{TenantID: "t", Scope: policy.RetentionScopeSource, MaxAgeDays: 1}, true},
		{"negative max_age", policy.RetentionPolicy{TenantID: "t", Scope: policy.RetentionScopeTenant, MaxAgeDays: -1}, true},
		{"valid tenant scope", policy.RetentionPolicy{TenantID: "t", Scope: policy.RetentionScopeTenant, MaxAgeDays: 30}, false},
		{"valid source scope", policy.RetentionPolicy{TenantID: "t", Scope: policy.RetentionScopeSource, ScopeValue: "src-1", MaxAgeDays: 7}, false},
		{"zero MaxAge ok (disabled)", policy.RetentionPolicy{TenantID: "t", Scope: policy.RetentionScopeTenant}, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.p.Validate()
			if c.err && err == nil {
				t.Fatalf("expected error")
			}
			if !c.err && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRetentionPolicy_IsExpired(t *testing.T) {
	t.Parallel()
	p := policy.RetentionPolicy{MaxAgeDays: 7}
	if p.IsExpired(time.Now().Add(-3 * 24 * time.Hour)) {
		t.Fatalf("3-day-old chunk should not be expired against 7-day policy")
	}
	if !p.IsExpired(time.Now().Add(-10 * 24 * time.Hour)) {
		t.Fatalf("10-day-old chunk should be expired against 7-day policy")
	}
	zero := policy.RetentionPolicy{MaxAgeDays: 0}
	if zero.IsExpired(time.Now().Add(-1000 * 24 * time.Hour)) {
		t.Fatalf("zero MaxAge means rule disabled")
	}
}

func TestEffective_TenantOnly(t *testing.T) {
	t.Parallel()
	rules := []policy.RetentionPolicy{
		{TenantID: "t", Scope: policy.RetentionScopeTenant, MaxAgeDays: 30},
	}
	got, ok := policy.Effective(policy.ChunkScope{TenantID: "t", SourceID: "s1"}, rules)
	if !ok || got.MaxAgeDays != 30 {
		t.Fatalf("expected 30, got %+v ok=%v", got, ok)
	}
}

func TestEffective_MoreSpecificWins_WhenMoreRestrictive(t *testing.T) {
	t.Parallel()
	rules := []policy.RetentionPolicy{
		{TenantID: "t", Scope: policy.RetentionScopeTenant, MaxAgeDays: 30},
		{TenantID: "t", Scope: policy.RetentionScopeSource, ScopeValue: "s1", MaxAgeDays: 7},
		{TenantID: "t", Scope: policy.RetentionScopeNamespace, ScopeValue: "s1/ns-a", MaxAgeDays: 1},
	}
	got, ok := policy.Effective(policy.ChunkScope{TenantID: "t", SourceID: "s1", NamespaceID: "ns-a"}, rules)
	if !ok || got.MaxAgeDays != 1 {
		t.Fatalf("expected 1 (most-restrictive), got %+v", got)
	}
}

func TestEffective_LooseLayerStillAppliesWhenSpecificDoesNotMatch(t *testing.T) {
	t.Parallel()
	rules := []policy.RetentionPolicy{
		{TenantID: "t", Scope: policy.RetentionScopeTenant, MaxAgeDays: 30},
		{TenantID: "t", Scope: policy.RetentionScopeSource, ScopeValue: "s2", MaxAgeDays: 7},
	}
	got, _ := policy.Effective(policy.ChunkScope{TenantID: "t", SourceID: "s1"}, rules)
	if got.MaxAgeDays != 30 {
		t.Fatalf("expected fallback to tenant scope (30), got %d", got.MaxAgeDays)
	}
}

func TestEffective_TenantIsolation(t *testing.T) {
	t.Parallel()
	rules := []policy.RetentionPolicy{
		{TenantID: "other", Scope: policy.RetentionScopeTenant, MaxAgeDays: 1},
	}
	if _, ok := policy.Effective(policy.ChunkScope{TenantID: "t"}, rules); ok {
		t.Fatalf("cross-tenant retention rule must be ignored")
	}
}

func TestEffective_NoRules(t *testing.T) {
	t.Parallel()
	if _, ok := policy.Effective(policy.ChunkScope{TenantID: "t"}, nil); ok {
		t.Fatalf("no rules → no policy applies")
	}
}

func TestIsExpired_ConvenienceWrapper(t *testing.T) {
	t.Parallel()
	rules := []policy.RetentionPolicy{{TenantID: "t", Scope: policy.RetentionScopeTenant, MaxAgeDays: 1}}
	if !policy.IsExpired(policy.ChunkScope{TenantID: "t"}, time.Now().Add(-2*24*time.Hour), rules) {
		t.Fatalf("expected expired")
	}
	if policy.IsExpired(policy.ChunkScope{TenantID: "t"}, time.Now(), rules) {
		t.Fatalf("expected not expired")
	}
}
