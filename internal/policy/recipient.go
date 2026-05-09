package policy

// RecipientRule is one allow/deny entry in a per-channel,
// per-skill RecipientPolicy. Empty SkillID means "applies to every
// skill"; that's the catch-all rule the admin portal renders at the
// top of the list.
type RecipientRule struct {
	// SkillID is the downstream consumer (e.g. "summarizer",
	// "qa-bot", "vector-export"). Empty matches every skill.
	SkillID string

	// Action is allow or deny. Same semantics as ACL: deny wins.
	Action ACLAction
}

// RecipientPolicy is the per-channel allow/deny list of downstream
// consumers. The retrieval handler filters results through this
// AFTER merge + rerank + ACL — i.e. the same matches may be visible
// to skill A but hidden from skill B if the channel's recipient
// policy denies B.
type RecipientPolicy struct {
	// TenantID, ChannelID scope the policy.
	TenantID  string
	ChannelID string

	// Rules is the list of allow/deny entries. Order is
	// informational — IsAllowed is order-independent because deny
	// always wins.
	Rules []RecipientRule

	// DefaultAllow controls the default verdict when no rule
	// matches. true = open-by-default, false = closed-by-default.
	// Reasonable platforms default open here; closed-by-default is
	// for high-security channels that must explicitly allow each
	// new skill.
	DefaultAllow bool
}

// IsAllowed reports whether skillID may receive matches from this
// channel. The semantics:
//
//  1. If ANY deny rule matches, return false.
//  2. Else, if any allow rule matches, return true.
//  3. Else, return DefaultAllow.
//
// A rule matches when its SkillID is empty (catch-all) or equals
// skillID exactly.
func (p *RecipientPolicy) IsAllowed(skillID string) bool {
	if p == nil {
		return true // no policy installed → open-by-default
	}
	matchedAllow := false
	for _, r := range p.Rules {
		if r.SkillID != "" && r.SkillID != skillID {
			continue
		}
		if r.Action == ACLActionDeny {
			return false
		}
		if r.Action == ACLActionAllow {
			matchedAllow = true
		}
	}
	if matchedAllow {
		return true
	}
	return p.DefaultAllow
}

// AllowedSkills returns the subset of skillIDs the policy admits.
// Convenience helper for callers (e.g. retrieval) that fan out to
// multiple skills in one call.
func (p *RecipientPolicy) AllowedSkills(skillIDs []string) []string {
	out := make([]string, 0, len(skillIDs))
	for _, id := range skillIDs {
		if p.IsAllowed(id) {
			out = append(out, id)
		}
	}
	return out
}
