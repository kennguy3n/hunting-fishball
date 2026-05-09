package retrieval

import (
	"sort"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// PrivacyStrip is the structured privacy disclosure attached to
// every RetrieveHit. Per docs/PROPOSAL.md §7, every result row
// already carries a `privacy_label`; the strip extends that with
// the additional context the client needs to render the privacy UI
// (mobile, desktop, admin portal) without re-deriving the policy
// state from the bare label.
//
// Field semantics:
//
//   - Mode: the effective privacy mode for the (tenant, channel)
//     that resolved the request. Echoed so a client can render the
//     "this answer was governed by mode X" line without inspecting
//     the response envelope.
//   - ProcessedWhere: the locality of the compute tier the chunk
//     went through. "on-device" is the strictest tier (no network
//     leaving the device), "local-api" is on-tenant compute, and
//     "remote" is third-party compute (e.g. OpenAI / Bedrock).
//   - ModelTier: which class of model touched the chunk during
//     retrieval / rerank. "none" means no model at all (BM25
//     fallback path); "local-slm" is the on-device Bonsai-style
//     SLM; "remote-llm" is the upstream LLM tier.
//   - DataSources: the connector types that contributed at least
//     one upstream document for the chunk (e.g. ["google-drive"]).
//     Plural so a chunk reconstructed from multiple sources (graph
//     traversal of related docs) reports every contributor.
//   - PolicyApplied: human-readable references to the policy rules
//     that fired during the gating decision. Empty list means the
//     default-allow path was taken.
type PrivacyStrip struct {
	Mode           string   `json:"mode"`
	ProcessedWhere string   `json:"processed_where"`
	ModelTier      string   `json:"model_tier"`
	DataSources    []string `json:"data_sources"`
	PolicyApplied  []string `json:"policy_applied"`
}

// BuildPrivacyStrip projects a Match + the resolved policy snapshot
// into the structured PrivacyStrip. The match's privacy_label
// (already populated by the storage layer) is the dominant signal
// for ProcessedWhere / ModelTier; the snapshot supplies Mode and
// PolicyApplied (the rule references that fired). Connector +
// per-stream sources are read from the match metadata.
//
// Wiring: the retrieval handler calls this once per Match after
// applyPolicySnapshot and before the response is returned. Cache
// hits also rebuild the strip — caching only the strip would
// double-count when the live policy changes between writes.
func BuildPrivacyStrip(match *Match, snapshot policy.PolicySnapshot) PrivacyStrip {
	if match == nil {
		return PrivacyStrip{
			Mode:           string(snapshot.EffectiveMode),
			ProcessedWhere: "unknown",
			ModelTier:      "none",
			DataSources:    []string{},
			PolicyApplied:  []string{},
		}
	}

	mode := string(snapshot.EffectiveMode)
	if mode == "" {
		mode = "unknown"
	}

	return PrivacyStrip{
		Mode:           mode,
		ProcessedWhere: classifyProcessedWhere(match.PrivacyLabel, snapshot.EffectiveMode),
		ModelTier:      classifyModelTier(match.PrivacyLabel, snapshot.EffectiveMode),
		DataSources:    dataSourcesFromMatch(match),
		PolicyApplied:  appliedRulesForMatch(match, snapshot),
	}
}

// classifyProcessedWhere maps the chunk's privacy label (or the
// effective mode when the label is empty) into the three locality
// buckets the client UI shows. The order maps the privacy ladder:
//
//	no-ai      → "blocked"
//	local-only → "on-device"
//	local-api  → "local-api"
//	hybrid     → "local-api"
//	remote     → "remote"
//
// Unknown labels collapse to the snapshot's effective mode so a
// chunk with a blank privacy_label can never be silently surfaced
// as "remote" — the strictest interpretation wins.
func classifyProcessedWhere(label string, mode policy.PrivacyMode) string {
	resolved := label
	if resolved == "" {
		resolved = string(mode)
	}
	switch policy.PrivacyMode(resolved) {
	case policy.PrivacyModeNoAI:
		return "blocked"
	case policy.PrivacyModeLocalOnly:
		return "on-device"
	case policy.PrivacyModeLocalAPI:
		return "local-api"
	case policy.PrivacyModeHybrid:
		return "local-api"
	case policy.PrivacyModeRemote:
		return "remote"
	default:
		return "unknown"
	}
}

// classifyModelTier names the model class that ran during
// retrieval / rerank. "none" is reserved for the BM25-only fallback
// path or for chunks blocked by no-ai mode. The mapping is
// intentionally conservative — a chunk with a "remote" privacy
// label could in principle have been processed by a remote LLM,
// but we report the tier the policy authorised, not the tier that
// actually ran (which is observable elsewhere via the audit log).
func classifyModelTier(label string, mode policy.PrivacyMode) string {
	resolved := label
	if resolved == "" {
		resolved = string(mode)
	}
	switch policy.PrivacyMode(resolved) {
	case policy.PrivacyModeNoAI:
		return "none"
	case policy.PrivacyModeLocalOnly, policy.PrivacyModeLocalAPI:
		return "local-slm"
	case policy.PrivacyModeHybrid, policy.PrivacyModeRemote:
		return "remote-llm"
	default:
		return "none"
	}
}

// dataSourcesFromMatch returns the connector types that contributed
// to the match. Today the storage layer surfaces the primary
// connector in match.Connector; if a match aggregates across
// multiple connectors a future code path can extend this without
// breaking the wire format. Empty connector falls back to the
// Match.Sources slice (the per-backend stream names: "vector",
// "bm25", ...) so a client always gets *something* in the
// DataSources field.
func dataSourcesFromMatch(m *Match) []string {
	if m == nil {
		return []string{}
	}
	if m.Connector != "" {
		return []string{m.Connector}
	}
	if len(m.Sources) > 0 {
		out := append([]string(nil), m.Sources...)
		sort.Strings(out)
		return out
	}
	return []string{}
}

// appliedRulesForMatch reports the policy rules that fired during
// the gating decision for this match. We report the active rule
// set (ACL + recipient) at the human-readable reference level so
// the client renders something like "acl[2]:allow/drive/**" — this
// is the same format used by simulator_conflict.formatACLRef.
//
// The implementation does NOT re-evaluate every rule (that would
// run an O(n) ACL match per response hit); instead it surfaces
// rules whose scope can plausibly cover the chunk's attributes.
// The exact "which rule won" attribution remains in the audit log,
// where it is recorded once per request.
func appliedRulesForMatch(m *Match, snap policy.PolicySnapshot) []string {
	out := []string{}
	if snap.ACL != nil {
		attrs := policy.ChunkAttrs{}
		if m != nil {
			attrs.SourceID = m.SourceID
			if path, ok := matchPath(m); ok {
				attrs.Path = path
			}
		}
		v := snap.ACL.Evaluate(attrs)
		if v.MatchedRule >= 0 && v.MatchedRule < len(snap.ACL.Rules) {
			r := snap.ACL.Rules[v.MatchedRule]
			out = append(out, formatACLRef(v.MatchedRule, r))
		}
	}
	if snap.Recipient != nil {
		// We don't know the calling skill at strip-build time —
		// the handler stamps it on the response envelope. Surface
		// only the recipient policy's existence so the client can
		// disclose "recipient policy in effect" without claiming
		// an outcome.
		if len(snap.Recipient.Rules) > 0 {
			out = append(out, "recipient_policy:active")
		}
	}
	return out
}

// matchFromCachedHit projects a public RetrieveHit into a *Match
// shape sufficient for BuildPrivacyStrip. Cached hits don't round-
// trip the full *Match (e.g. Sources, IngestedAt are dropped to
// keep the cache compact), but the privacy strip only needs the
// chunk's identity, privacy_label, connector, and metadata, which
// are all preserved.
func matchFromCachedHit(h RetrieveHit) *Match {
	return &Match{
		ID:           h.ID,
		Score:        h.Score,
		TenantID:     h.TenantID,
		SourceID:     h.SourceID,
		DocumentID:   h.DocumentID,
		BlockID:      h.BlockID,
		Title:        h.Title,
		URI:          h.URI,
		Text:         h.Text,
		PrivacyLabel: h.PrivacyLabel,
		Connector:    h.Connector,
		Metadata:     h.Metadata,
		Sources:      h.Sources,
	}
}

// matchPath extracts a path-like string from a Match. Different
// backend adapters stuff the path in different metadata keys; we
// probe the common ones and return the first hit so the ACL
// evaluator has something to glob against.
func matchPath(m *Match) (string, bool) {
	if m == nil || m.Metadata == nil {
		return "", false
	}
	for _, key := range []string{"path", "uri_path", "doc_path"} {
		if v, ok := m.Metadata[key]; ok {
			if s, sok := v.(string); sok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// formatACLRef mirrors the policy package's format so privacy
// strips share the same rule-reference shape as simulator
// conflicts. Kept private to retrieval to avoid widening the
// policy package's API surface.
func formatACLRef(idx int, r policy.ACLRule) string {
	glob := r.PathGlob
	if glob == "" {
		glob = "*"
	}
	return "acl[" + itoa(idx) + "]:" + string(r.Action) + "/" + glob
}

// itoa is the small-int formatter used by formatACLRef. Avoids
// pulling fmt into the hot retrieval path for a single-line
// rendering.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
