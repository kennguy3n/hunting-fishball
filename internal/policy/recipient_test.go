package policy_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestRecipient_NilPolicyAllows(t *testing.T) {
	t.Parallel()
	var p *policy.RecipientPolicy
	if !p.IsAllowed("summarizer") {
		t.Fatal("nil policy must default-allow")
	}
}

func TestRecipient_DefaultAllowOpenByDefault(t *testing.T) {
	t.Parallel()
	p := &policy.RecipientPolicy{DefaultAllow: true}
	if !p.IsAllowed("anything") {
		t.Fatal("DefaultAllow=true must admit every skill")
	}
}

func TestRecipient_DefaultDenyClosedByDefault(t *testing.T) {
	t.Parallel()
	p := &policy.RecipientPolicy{DefaultAllow: false}
	if p.IsAllowed("rogue-skill") {
		t.Fatal("DefaultAllow=false must reject unlisted skills")
	}
}

func TestRecipient_DenyOverridesCatchAllAllow(t *testing.T) {
	t.Parallel()
	p := &policy.RecipientPolicy{
		Rules: []policy.RecipientRule{
			{Action: policy.ACLActionAllow}, // catch-all allow
			{SkillID: "exfil", Action: policy.ACLActionDeny},
		},
		DefaultAllow: false,
	}
	if !p.IsAllowed("summarizer") {
		t.Fatal("catch-all allow must admit summarizer")
	}
	if p.IsAllowed("exfil") {
		t.Fatal("explicit deny must override catch-all allow")
	}
}

func TestRecipient_AllowedSkillsFiltersList(t *testing.T) {
	t.Parallel()
	p := &policy.RecipientPolicy{
		Rules: []policy.RecipientRule{
			{SkillID: "summarizer", Action: policy.ACLActionAllow},
			{SkillID: "exfil", Action: policy.ACLActionDeny},
		},
		DefaultAllow: false,
	}
	got := p.AllowedSkills([]string{"summarizer", "exfil", "translation"})
	if len(got) != 1 || got[0] != "summarizer" {
		t.Fatalf("AllowedSkills: %v", got)
	}
}

func TestRecipient_RuleSpecificityCatchAllDenyDeniesAll(t *testing.T) {
	t.Parallel()
	p := &policy.RecipientPolicy{
		Rules: []policy.RecipientRule{
			{Action: policy.ACLActionDeny},
		},
		DefaultAllow: true,
	}
	if p.IsAllowed("anything") {
		t.Fatal("catch-all deny must reject every skill")
	}
}
