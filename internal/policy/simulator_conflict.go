package policy

import (
	"fmt"
	"sort"
)

// ConflictSeverity classifies a PolicyConflict. "error"-severity
// conflicts MUST block draft promotion; "warning"-severity conflicts
// surface in the admin portal but do not gate promotion.
type ConflictSeverity string

const (
	// ConflictSeverityError gates promotion. Used for contradictions
	// that would produce undefined behaviour at retrieval time
	// (e.g. an ACL rule that both allows and denies the same path).
	ConflictSeverityError ConflictSeverity = "error"

	// ConflictSeverityWarning surfaces a confusing but resolvable
	// configuration. Used for overlapping rules that the
	// deny-over-allow precedence already disambiguates.
	ConflictSeverityWarning ConflictSeverity = "warning"
)

// ConflictType is the categorical label rendered in the admin
// portal next to each PolicyConflict. Exported so the front-end can
// branch on stable string values rather than parsing the
// Description field.
type ConflictType string

const (
	// ConflictTypePrivacyModeOverride flags an effective-mode
	// collapse: the simulator computes EffectiveMode and surfaces
	// it when the configured tenant + channel modes disagree
	// dramatically.
	ConflictTypePrivacyModeOverride ConflictType = "privacy_mode_override"

	// ConflictTypeACLOverlap flags two ACL rules that match the
	// same chunk attributes with opposing actions, or whose globs
	// are nested.
	ConflictTypeACLOverlap ConflictType = "acl_overlap"

	// ConflictTypeRecipientContradiction flags two recipient rules
	// where one allows a skill and another denies the same skill
	// (or denies via the catch-all and then allows the same
	// skill explicitly, which deny-over-allow would silently
	// reject).
	ConflictTypeRecipientContradiction ConflictType = "recipient_contradiction"
)

// PolicyConflict is one issue surfaced by DetectConflicts. The
// simulator returns these in a deterministic order so callers can
// snapshot them in tests; ordering is by (Severity, Type,
// Description) — errors first.
type PolicyConflict struct {
	// Severity is "error" (blocks promotion) or "warning"
	// (surfaces but allows promotion).
	Severity ConflictSeverity `json:"severity"`

	// Type is a stable categorical label.
	Type ConflictType `json:"type"`

	// Description is a human-readable narrative for the admin UI.
	Description string `json:"description"`

	// RuleA / RuleB reference the conflicting rules in a form a
	// reviewer can locate. Format follows the existing admin
	// portal conventions: "tenant:<mode>", "acl[3]:<glob>",
	// "recipient[1]:<skill>".
	RuleA string `json:"rule_a,omitempty"`
	RuleB string `json:"rule_b,omitempty"`
}

// DetectConflicts inspects a candidate PolicySnapshot and surfaces
// every conflict that would either gate promotion (severity=error)
// or warrant a warning (severity=warning). The function is
// deterministic — given the same input it returns the same list in
// the same order — so admin-portal snapshot tests are stable.
//
// Detection rules:
//
//   - Privacy mode override: when EffectiveMode is "no-ai" but the
//     ACL or recipient list contains rules that would otherwise
//     unblock content, the no-ai mode silently overrides them.
//     Surfaced as a warning so admins notice the dead config.
//   - ACL contradictions: when two rules match identical chunk
//     attributes with opposing actions, the deny-over-allow
//     precedence wins, but the allow rule is dead. Surfaced as an
//     error.
//   - ACL nested-glob warnings: when a deny rule's path glob
//     subsumes an allow rule's path glob (or vice versa), one of
//     the two is partially dead. Surfaced as a warning.
//   - Recipient contradictions: when an explicit skill rule
//     conflicts with the catch-all rule (e.g. catch-all allow +
//     specific deny is fine, but catch-all deny + specific allow
//     is silently denied by the deny-over-allow short-circuit in
//     RecipientPolicy.IsAllowed). The latter is an error.
func DetectConflicts(draft PolicySnapshot) []PolicyConflict {
	var out []PolicyConflict

	out = append(out, detectPrivacyModeOverride(draft)...)
	out = append(out, detectACLConflicts(draft)...)
	out = append(out, detectRecipientConflicts(draft)...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			// Errors sort before warnings.
			return out[i].Severity == ConflictSeverityError
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Description < out[j].Description
	})
	return out
}

// HasErrors reports whether any conflict in the slice has
// severity=error. The promotion workflow uses this as the gate.
func HasErrors(conflicts []PolicyConflict) bool {
	for _, c := range conflicts {
		if c.Severity == ConflictSeverityError {
			return true
		}
	}
	return false
}

// detectPrivacyModeOverride flags configurations where EffectiveMode
// silently invalidates the rest of the policy. Today the only check
// is "no-ai" mode with non-trivial ACL or recipient configuration —
// EffectiveMode short-circuits retrieval before either gate runs, so
// the rules are dead config.
func detectPrivacyModeOverride(draft PolicySnapshot) []PolicyConflict {
	if draft.EffectiveMode != PrivacyModeNoAI {
		return nil
	}
	hasACL := draft.ACL != nil && len(draft.ACL.Rules) > 0
	hasRecipient := draft.Recipient != nil && len(draft.Recipient.Rules) > 0
	if !hasACL && !hasRecipient {
		return nil
	}
	desc := "effective mode 'no-ai' will short-circuit retrieval before ACL and recipient rules apply; the rules below are dead config"
	return []PolicyConflict{{
		Severity:    ConflictSeverityWarning,
		Type:        ConflictTypePrivacyModeOverride,
		Description: desc,
		RuleA:       "tenant:no-ai",
	}}
}

// detectACLConflicts walks every pair of ACL rules and flags both
// outright contradictions (same attrs, opposing actions) and
// nested-glob overlaps (one rule's glob is a strict superset of the
// other's). The walk is O(n²) in the rule count; the production
// rule sets are short (<100 per tenant) so this is well within the
// admin-handler latency budget.
func detectACLConflicts(draft PolicySnapshot) []PolicyConflict {
	if draft.ACL == nil || len(draft.ACL.Rules) < 2 {
		return nil
	}
	rules := draft.ACL.Rules
	var out []PolicyConflict
	for i := 0; i < len(rules); i++ {
		for j := i + 1; j < len(rules); j++ {
			a, b := rules[i], rules[j]
			if !aclScopesOverlap(a, b) {
				continue
			}
			switch {
			case identicalScope(a, b) && a.Action != b.Action:
				out = append(out, PolicyConflict{
					Severity:    ConflictSeverityError,
					Type:        ConflictTypeACLOverlap,
					Description: fmt.Sprintf("ACL rules at index %d and %d cover identical attributes with opposing actions; deny-over-allow makes the allow dead", i, j),
					RuleA:       formatACLRef(i, a),
					RuleB:       formatACLRef(j, b),
				})
			case nestedGlob(a.PathGlob, b.PathGlob) && a.Action != b.Action:
				out = append(out, PolicyConflict{
					Severity:    ConflictSeverityWarning,
					Type:        ConflictTypeACLOverlap,
					Description: fmt.Sprintf("ACL rules at index %d and %d have nested path globs with opposing actions; the inner rule may be partially shadowed", i, j),
					RuleA:       formatACLRef(i, a),
					RuleB:       formatACLRef(j, b),
				})
			}
		}
	}
	return out
}

// aclScopesOverlap reports whether two ACL rules could match a
// common chunk. The check is conservative: when SourceID or
// NamespaceID disagree (and neither is empty/wildcard), the rules
// are guaranteed disjoint and the conflict walker can skip them.
func aclScopesOverlap(a, b ACLRule) bool {
	if a.SourceID != "" && b.SourceID != "" && a.SourceID != b.SourceID {
		return false
	}
	if a.NamespaceID != "" && b.NamespaceID != "" && a.NamespaceID != b.NamespaceID {
		return false
	}
	return true
}

// identicalScope reports whether two ACL rules cover EXACTLY the
// same chunk attributes — same SourceID, NamespaceID, and
// PathGlob (string-equality, not glob-semantics). Used by
// detectACLConflicts to surface the strongest case of conflict.
func identicalScope(a, b ACLRule) bool {
	return a.SourceID == b.SourceID && a.NamespaceID == b.NamespaceID && a.PathGlob == b.PathGlob
}

// nestedGlob reports whether one of (outer, inner) is a strict
// prefix-or-superset of the other in glob-semantic terms. Examples:
//
//   - "a/**" / "a/b/c"           → true (outer matches the inner)
//   - "drive/secret/**" / "drive/secret/2024/q1" → true
//   - "a/*" / "a/b"              → true (single-segment wildcard)
//   - "a/**" / "a/**"            → false (identical, not nested)
//
// The implementation reuses the package's globMatch helper: a glob
// G subsumes target T iff G is more permissive than T-as-a-glob —
// approximated by treating T as a literal and asking whether G
// matches it. This is conservative: it may miss subtle cases where
// both globs match a third path but neither matches the other (e.g.
// `a/*/x` and `a/y/*`); those surface as a warning only when an
// admin runs the simulator's what-if retrieval against the corpus.
func nestedGlob(outer, inner string) bool {
	if outer == "" || inner == "" {
		return false
	}
	if outer == inner {
		return false
	}
	if globMatch(outer, inner) || globMatch(inner, outer) {
		return true
	}
	return false
}

// formatACLRef renders an ACL rule pointer in the canonical "acl[i]:
// <glob>" admin-portal format.
func formatACLRef(idx int, r ACLRule) string {
	glob := r.PathGlob
	if glob == "" {
		glob = "*"
	}
	return fmt.Sprintf("acl[%d]:%s/%s", idx, r.Action, glob)
}

// detectRecipientConflicts surfaces contradictions in the recipient
// rule list. The semantic model is RecipientPolicy.IsAllowed —
// deny-over-allow plus a per-channel default. Two patterns produce
// dead config:
//
//   - Catch-all deny + specific allow: the catch-all matches every
//     skill including the explicitly-allowed one, so the explicit
//     allow is silently denied. ERROR — administrator likely
//     intended the explicit allow to override.
//   - Catch-all allow + specific deny: deny-over-allow correctly
//     denies the specific skill; the allow stands for everyone
//     else. Surfaced as a WARNING for visibility, not an error.
func detectRecipientConflicts(draft PolicySnapshot) []PolicyConflict {
	if draft.Recipient == nil || len(draft.Recipient.Rules) < 2 {
		return nil
	}
	rules := draft.Recipient.Rules
	var (
		catchAllAllow = -1
		catchAllDeny  = -1
	)
	for i, r := range rules {
		if r.SkillID != "" {
			continue
		}
		switch r.Action {
		case ACLActionAllow:
			if catchAllAllow == -1 {
				catchAllAllow = i
			}
		case ACLActionDeny:
			if catchAllDeny == -1 {
				catchAllDeny = i
			}
		}
	}

	var out []PolicyConflict
	for i, r := range rules {
		if r.SkillID == "" {
			continue
		}
		switch r.Action {
		case ACLActionAllow:
			if catchAllDeny >= 0 {
				out = append(out, PolicyConflict{
					Severity:    ConflictSeverityError,
					Type:        ConflictTypeRecipientContradiction,
					Description: fmt.Sprintf("recipient rule at index %d explicitly allows skill %q but a catch-all deny at index %d denies it first (deny-over-allow)", i, r.SkillID, catchAllDeny),
					RuleA:       formatRecipientRef(i, r),
					RuleB:       formatRecipientRef(catchAllDeny, rules[catchAllDeny]),
				})
			}
		case ACLActionDeny:
			if catchAllAllow >= 0 {
				out = append(out, PolicyConflict{
					Severity:    ConflictSeverityWarning,
					Type:        ConflictTypeRecipientContradiction,
					Description: fmt.Sprintf("recipient rule at index %d denies skill %q while a catch-all allow at index %d covers everyone; deny-over-allow handles the override but the allow rule is partially shadowed", i, r.SkillID, catchAllAllow),
					RuleA:       formatRecipientRef(i, r),
					RuleB:       formatRecipientRef(catchAllAllow, rules[catchAllAllow]),
				})
			}
		}
	}
	return out
}

// formatRecipientRef renders a recipient rule pointer in the
// "recipient[i]:<action>:<skill>" admin-portal format. Catch-all
// rules render with skill="*".
func formatRecipientRef(idx int, r RecipientRule) string {
	skill := r.SkillID
	if skill == "" {
		skill = "*"
	}
	return fmt.Sprintf("recipient[%d]:%s:%s", idx, r.Action, skill)
}
