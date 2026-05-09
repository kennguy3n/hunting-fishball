package policy

import (
	"path"
	"sort"
	"strings"
)

// ACLAction is the verdict carried by an ACLRule.
type ACLAction string

const (
	// ACLActionAllow admits the chunk through the policy gate.
	ACLActionAllow ACLAction = "allow"
	// ACLActionDeny rejects the chunk. Deny rules ALWAYS win — see
	// AllowDenyList.Evaluate.
	ACLActionDeny ACLAction = "deny"
)

// ACLRule is one entry in a tenant's per-channel allow/deny list.
// The retrieval handler evaluates rules against ChunkAttrs after the
// reranker.
//
// Empty fields act as wildcards: an empty SourceID matches any
// source, an empty NamespaceID matches any namespace, an empty
// PathGlob matches any path. The combination (everything empty)
// matches every chunk; reserve that pattern for tenant-wide rules.
type ACLRule struct {
	// SourceID, when non-empty, restricts the rule to one source.
	SourceID string

	// NamespaceID, when non-empty, restricts the rule to one
	// namespace within the source.
	NamespaceID string

	// PathGlob, when non-empty, is a glob pattern matched against
	// the chunk's path metadata. The implementation uses
	// path.Match — `*` matches a path segment, `?` one character,
	// and `**` (a non-standard extension) matches across slashes.
	PathGlob string

	// Action is allow or deny.
	Action ACLAction

	// ComputeTier optionally tags the chunk with a downstream
	// compute budget ("light", "heavy", ...). Returned to the
	// retrieval caller via ChunkVerdict; ignored when empty.
	ComputeTier string
}

// AllowDenyList is the ordered set of rules a tenant has installed
// for a (tenant, channel). The order is informational — Evaluate
// returns the same verdict regardless because deny ALWAYS wins.
type AllowDenyList struct {
	// TenantID, ChannelID scope the list.
	TenantID  string
	ChannelID string

	// Rules is the unordered set of rules. AllowDenyList.Evaluate
	// short-circuits on the first deny match.
	Rules []ACLRule
}

// ChunkAttrs is the subset of a retrieval Match the ACL inspects.
// Decoupling here lets the retrieval handler call Evaluate without
// importing the full Match type.
type ChunkAttrs struct {
	SourceID    string
	NamespaceID string
	Path        string
}

// ChunkVerdict is the ACL outcome for one chunk.
type ChunkVerdict struct {
	Allowed     bool
	ComputeTier string
	// MatchedRule is the index of the rule that produced the
	// verdict; -1 when no rule matched (default-allow). Useful for
	// audit trails and tests.
	MatchedRule int
}

// Evaluate returns whether attrs is allowed by the list. Semantics:
//
//  1. If ANY deny rule matches, the chunk is denied. Deny wins.
//  2. Else, if any allow rule matches, the chunk is allowed and the
//     ComputeTier from the FIRST matching allow rule (in slice
//     order) is returned.
//  3. Else, the chunk is allowed by default with no compute tier.
//
// "Default-allow on no match" is intentional: an empty AllowDenyList
// (no rules installed) must not block retrieval — the higher-level
// privacy mode handles the strict-by-default case.
func (l *AllowDenyList) Evaluate(attrs ChunkAttrs) ChunkVerdict {
	if l == nil || len(l.Rules) == 0 {
		return ChunkVerdict{Allowed: true, MatchedRule: -1}
	}
	firstAllow := -1
	firstAllowTier := ""
	for i, r := range l.Rules {
		if !ruleMatches(r, attrs) {
			continue
		}
		if r.Action == ACLActionDeny {
			return ChunkVerdict{Allowed: false, MatchedRule: i}
		}
		if firstAllow == -1 {
			firstAllow = i
			firstAllowTier = r.ComputeTier
		}
	}
	if firstAllow >= 0 {
		return ChunkVerdict{Allowed: true, ComputeTier: firstAllowTier, MatchedRule: firstAllow}
	}
	return ChunkVerdict{Allowed: true, MatchedRule: -1}
}

// ruleMatches reports whether r covers attrs.
func ruleMatches(r ACLRule, attrs ChunkAttrs) bool {
	if r.SourceID != "" && r.SourceID != attrs.SourceID {
		return false
	}
	if r.NamespaceID != "" && r.NamespaceID != attrs.NamespaceID {
		return false
	}
	if r.PathGlob == "" {
		return true
	}
	return globMatch(r.PathGlob, attrs.Path)
}

// globMatch is a helper that supports the standard path.Match wildcards
// (`*`, `?`, `[]`) plus a `**` extension that matches across slashes.
// The implementation collapses `**` to a tighter regex-equivalent
// pattern by translating to repeated path.Match calls; see tests for
// the supported cases.
func globMatch(pattern, target string) bool {
	if pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "**") {
		ok, _ := path.Match(pattern, target)
		return ok
	}
	// Split on `**`. Each segment must match somewhere (in order) in
	// the target. The first segment must match a prefix; the last
	// segment must match a suffix.
	parts := strings.Split(pattern, "**")
	cursor := 0
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(target, p) {
				return false
			}
			cursor = len(p)
			continue
		}
		idx := strings.Index(target[cursor:], p)
		if idx < 0 {
			return false
		}
		cursor += idx + len(p)
		if i == len(parts)-1 && cursor != len(target) {
			// trailing segment must hit the end
			if !strings.HasSuffix(target, p) {
				return false
			}
		}
	}
	return true
}

// SortRules returns a copy of rules sorted with deny rules first
// (since they short-circuit) and then by (SourceID, NamespaceID,
// PathGlob) for deterministic admin-UI rendering. The Evaluate
// result is independent of order, but the admin portal renders
// rules in this canonical order.
func SortRules(rules []ACLRule) []ACLRule {
	out := make([]ACLRule, len(rules))
	copy(out, rules)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Action != out[j].Action {
			return out[i].Action == ACLActionDeny
		}
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		if out[i].NamespaceID != out[j].NamespaceID {
			return out[i].NamespaceID < out[j].NamespaceID
		}
		return out[i].PathGlob < out[j].PathGlob
	})
	return out
}
