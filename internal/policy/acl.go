package policy

import (
	"regexp"
	"sort"
	"strings"
	"sync"
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
	// the chunk's path metadata. Wildcards: `*` matches a single
	// segment (no `/`), `?` one non-`/` character, `[abc]` /
	// `[!abc]` a character class, and `**` matches across `/`.
	// Other regex metacharacters in the pattern are treated as
	// literals — see globMatch.
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

// globMatch reports whether target satisfies the glob pattern.
//
// Supported metacharacters:
//   - `**` matches any run of characters, including `/` (cross-segment).
//   - `*`  matches any run of non-`/` characters (single segment).
//   - `?`  matches a single non-`/` character.
//   - `[abc]` / `[!abc]` character classes (the `!` form is rewritten
//     to the regex `^` form).
//
// The pattern is translated to an anchored regular expression once
// per process and cached; subsequent calls reuse the compiled regex.
// All other regex metacharacters in the pattern are escaped, so
// punctuation in real-world paths (`.`, `+`, `(`, `)`, ...) matches
// as a literal.
//
// A pattern that fails to compile (e.g. an unbalanced character class
// the translator can't repair) returns false — fail-closed for the
// deny gate.
func globMatch(pattern, target string) bool {
	if pattern == "" {
		return true
	}
	re := compileGlob(pattern)
	if re == nil {
		return false
	}
	return re.MatchString(target)
}

// globRegexCache memoizes pattern → compiled regex (or nil for
// unparsable patterns) so each ACL rule's glob is translated at most
// once per process. sync.Map is used because the cache is
// write-once-per-pattern and dominated by reads on hot paths.
var globRegexCache sync.Map // string → *regexp.Regexp

func compileGlob(pattern string) *regexp.Regexp {
	if v, ok := globRegexCache.Load(pattern); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re
		}
		return nil
	}
	re, err := regexp.Compile("^" + globToRegex(pattern) + "$")
	if err != nil {
		globRegexCache.Store(pattern, (*regexp.Regexp)(nil))
		return nil
	}
	globRegexCache.Store(pattern, re)
	return re
}

// Sentinel placeholders for the `**` boundary forms. NUL is never a
// legal byte in a path glob, so using it for the placeholder lets us
// recognise tokens unambiguously in the second pass. Order of the
// substitutions in globToRegex matters: longer/more-specific forms
// must be replaced first so a `/**/` doesn't get split into `/**` +
// trailing `/`.
const (
	tokDoubleSlash         = "\x00d\x00" // /**/  → /(?:.*/)?
	tokDoubleSlashLeading  = "\x00l\x00" // **/   → (?:.*/)?
	tokDoubleSlashTrailing = "\x00t\x00" // /**   → (?:/.*)?
	tokDoubleStar          = "\x00s\x00" // **    → .*
)

// globToRegex translates a glob pattern to a regex source string
// (without the `^`/`$` anchors). Two passes:
//
//  1. Replace the four `**` boundary forms with sentinel tokens so
//     the slash-eating semantics survive the byte-level walk.
//     `/**/` matches zero or more middle segments (so `a/**/b` hits
//     both `a/b` and `a/x/y/b`); `**/x` matches `x` plus any nested
//     form; `x/**` matches `x` plus any deeper form.
//  2. Walk byte-by-byte translating tokens, single-char wildcards,
//     and character classes. Every other byte is passed through
//     regexp.QuoteMeta so punctuation in real paths (`.`, `+`, `(`,
//     ...) matches as a literal.
//
// The algorithm is byte-level rather than rune-level because the
// glob metacharacters are all ASCII; non-ASCII bytes pass through
// QuoteMeta unchanged.
func globToRegex(pattern string) string {
	p := pattern
	p = strings.ReplaceAll(p, "/**/", tokDoubleSlash)
	p = strings.ReplaceAll(p, "**/", tokDoubleSlashLeading)
	p = strings.ReplaceAll(p, "/**", tokDoubleSlashTrailing)
	p = strings.ReplaceAll(p, "**", tokDoubleStar)

	var b strings.Builder
	b.Grow(len(p) * 2)
	i := 0
	for i < len(p) {
		c := p[i]
		if c == 0 && i+2 < len(p) && p[i+2] == 0 {
			switch p[i+1] {
			case 'd':
				b.WriteString("/(?:.*/)?")
			case 'l':
				b.WriteString("(?:.*/)?")
			case 't':
				b.WriteString("(?:/.*)?")
			case 's':
				b.WriteString(".*")
			}
			i += 3
			continue
		}
		switch c {
		case '*':
			b.WriteString("[^/]*")
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		case '[':
			// Pass-through character class. Convert leading `!` to
			// regex negation `^`. Unclosed `[` is treated as a
			// literal `[` so we never produce an invalid regex.
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				i++
				continue
			}
			class := p[i+1 : i+1+end]
			b.WriteByte('[')
			if strings.HasPrefix(class, "!") {
				b.WriteByte('^')
				class = class[1:]
			}
			b.WriteString(class)
			b.WriteByte(']')
			i += end + 2
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	return b.String()
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
