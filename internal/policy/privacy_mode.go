// Package policy contains the Phase 4 multi-tenant policy framework:
// privacy modes, ACL allow/deny lists, and recipient policies. The
// retrieval handler consults this package before fan-out (privacy
// mode → backend selection) and after rerank (ACL + recipient →
// drop disallowed chunks).
//
// All policy state lives in Postgres so multiple replicas observe
// the same decisions; in-process caching is safe because policy
// edits emit `policy.edited` audit events that backend replicas can
// invalidate on.
package policy

import "strings"

// PrivacyMode describes the AI processing budget a tenant or channel
// is willing to grant. Modes are ordered from MOST restrictive
// (NoAI) to LEAST restrictive (Remote). The retrieval pipeline picks
// the STRICTER of (tenantMode, channelMode) for any given query —
// see EffectiveMode.
//
// The strings match the wire format used by ai-agent-context-engine
// and uneycom/b2c-kchat-portal so policy rows round-trip cleanly
// through both planes.
type PrivacyMode string

const (
	// PrivacyModeNoAI disables AI features entirely. Retrieval is
	// disallowed; the handler returns an empty result with a
	// PrivacyMode label so the client can render the disabled state.
	PrivacyModeNoAI PrivacyMode = "no-ai"

	// PrivacyModeLocalOnly limits processing to on-device models. No
	// data leaves the device — used by air-gapped deployments.
	PrivacyModeLocalOnly PrivacyMode = "local-only"

	// PrivacyModeLocalAPI permits the local API plane (within the
	// tenant's environment) but blocks public/external models.
	PrivacyModeLocalAPI PrivacyMode = "local-api"

	// PrivacyModeHybrid permits a mix of local and remote inference.
	// External models receive only the chunks the local model
	// requested as additional context.
	PrivacyModeHybrid PrivacyMode = "hybrid"

	// PrivacyModeRemote permits unrestricted use of remote models.
	// This is the most permissive mode.
	PrivacyModeRemote PrivacyMode = "remote"
)

// orderedModes is the strictness ordering: index 0 is the strictest.
// EffectiveMode returns whichever of the two inputs has the lower
// index (= stricter).
var orderedModes = []PrivacyMode{
	PrivacyModeNoAI,
	PrivacyModeLocalOnly,
	PrivacyModeLocalAPI,
	PrivacyModeHybrid,
	PrivacyModeRemote,
}

// AllPrivacyModes returns the canonical, strict-to-permissive list.
// Useful for admin dropdowns and validation.
func AllPrivacyModes() []PrivacyMode {
	out := make([]PrivacyMode, len(orderedModes))
	copy(out, orderedModes)
	return out
}

// IsValidPrivacyMode reports whether s names one of the known modes.
// Comparison is case-insensitive and tolerates `_`-vs-`-` variants.
func IsValidPrivacyMode(s string) bool {
	_, ok := normalisePrivacyMode(s)
	return ok
}

// ParsePrivacyMode normalises s to a known PrivacyMode. Returns
// (mode, true) on success or ("", false) when s is unknown.
func ParsePrivacyMode(s string) (PrivacyMode, bool) {
	return normalisePrivacyMode(s)
}

func normalisePrivacyMode(s string) (PrivacyMode, bool) {
	canon := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "-"))
	for _, m := range orderedModes {
		if string(m) == canon {
			return m, true
		}
	}
	return "", false
}

// rankOf returns the strictness rank of m. Strictest (NoAI) → 0;
// least strict (Remote) → len-1; unknown → len (treated as more
// strict than every known mode so unknown values fail closed).
func rankOf(m PrivacyMode) int {
	for i, om := range orderedModes {
		if om == m {
			return i
		}
	}
	return -1 // unknown
}

// EffectiveMode returns the stricter of (tenantMode, channelMode).
// When either input is unknown, the function returns the other
// (defaulting to NoAI when both are unknown). This guarantees
// fail-closed semantics: an unknown channel can never widen the
// tenant's policy.
func EffectiveMode(tenantMode, channelMode PrivacyMode) PrivacyMode {
	tIdx, cIdx := rankOf(tenantMode), rankOf(channelMode)
	switch {
	case tIdx < 0 && cIdx < 0:
		return PrivacyModeNoAI
	case tIdx < 0:
		return channelMode
	case cIdx < 0:
		return tenantMode
	case tIdx <= cIdx:
		return tenantMode
	default:
		return channelMode
	}
}

// AllowsAtLeast reports whether `actual` is at least as permissive
// as `required`. Used by the retrieval handler to decide whether a
// channel's effective mode is high enough to satisfy a per-skill
// requirement (e.g. summarisation needs Hybrid+).
func AllowsAtLeast(actual, required PrivacyMode) bool {
	a, r := rankOf(actual), rankOf(required)
	if a < 0 || r < 0 {
		return false
	}
	return a >= r
}
