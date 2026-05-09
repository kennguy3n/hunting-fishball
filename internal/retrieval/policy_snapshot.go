package retrieval

import (
	"context"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// PolicySnapshot captures every policy decision the retrieval
// handler needs for one (tenant, channel) at request time. The
// resolver loads it from Postgres (or an in-process cache); the
// handler treats it as immutable.
type PolicySnapshot struct {
	// EffectiveMode is the strict-of (tenantMode, channelMode). The
	// retrieval handler passes its string form to the existing
	// privacy-label PolicyFilter.
	EffectiveMode policy.PrivacyMode

	// ACL is the per-(tenant, channel) allow/deny list. nil → no
	// ACL installed (default-allow).
	ACL *policy.AllowDenyList

	// Recipient is the per-(tenant, channel) recipient policy. nil
	// → default-allow for every skill.
	Recipient *policy.RecipientPolicy
}

// PolicyResolver resolves a PolicySnapshot for a (tenant, channel)
// pair. Implementations live in cmd/api/main.go (Postgres-backed) or
// in tests (in-memory). When the resolver returns an error, the
// handler MUST fail closed — return zero hits with PrivacyMode set
// to NoAI — rather than silently widening the policy.
type PolicyResolver interface {
	Resolve(ctx context.Context, tenantID, channelID string) (PolicySnapshot, error)
}

// applyPolicySnapshot filters allowed against the snapshot's ACL +
// recipient policy. Returns (kept, blockedByACL, blockedByRecipient).
//
// skillID is the calling skill (e.g. "summarizer"); empty means
// "no specific skill" — recipient rules with explicit SkillIDs are
// skipped, only catch-all rules apply.
//
// The chunks' source_id, namespace_id, and (when set) `path`
// metadata feed into the ACL evaluation.
func applyPolicySnapshot(allowed []*Match, snap PolicySnapshot, skillID string) ([]*Match, int, int) {
	if len(allowed) == 0 {
		return allowed, 0, 0
	}
	// Recipient gate first — when the calling skill is denied
	// outright, the entire result set is dropped.
	if snap.Recipient != nil && !snap.Recipient.IsAllowed(skillID) {
		return nil, 0, len(allowed)
	}
	if snap.ACL == nil || len(snap.ACL.Rules) == 0 {
		return allowed, 0, 0
	}
	out := make([]*Match, 0, len(allowed))
	blockedByACL := 0
	for _, m := range allowed {
		if m == nil {
			continue
		}
		path, _ := metadataString(m.Metadata, "path")
		nsID, _ := metadataString(m.Metadata, "namespace_id")
		v := snap.ACL.Evaluate(policy.ChunkAttrs{
			SourceID: m.SourceID, NamespaceID: nsID, Path: path,
		})
		if v.Allowed {
			out = append(out, m)
		} else {
			blockedByACL++
		}
	}
	return out, blockedByACL, 0
}

// filterCachedBySnapshot re-applies the Phase 4 ACL + recipient gates
// to a CachedResult on the cache-hit path. The retrieval cache TTL is
// minutes-long, so a policy change between cache write and read MUST
// be re-evaluated against the cached chunks rather than served stale.
//
// Returns the (possibly trimmed) CachedResult, the count blocked by
// ACL, and the count blocked by recipient policy. When the recipient
// gate denies the calling skill, the entire result set is dropped.
//
// The implementation projects each CachedHit into a minimal Match
// shadow so applyPolicySnapshot is the single source of truth for the
// gate semantics.
func filterCachedBySnapshot(c *storage.CachedResult, snap PolicySnapshot, skillID string) (*storage.CachedResult, int, int) {
	if c == nil || len(c.Hits) == 0 {
		return c, 0, 0
	}
	shadows := make([]*Match, len(c.Hits))
	for i, h := range c.Hits {
		shadows[i] = &Match{
			ID:       h.ID,
			SourceID: h.SourceID,
			Metadata: h.Metadata,
		}
	}
	kept, blockedACL, blockedRecip := applyPolicySnapshot(shadows, snap, skillID)
	if blockedACL == 0 && blockedRecip == 0 {
		return c, 0, 0
	}
	keep := make(map[string]struct{}, len(kept))
	for _, m := range kept {
		keep[m.ID] = struct{}{}
	}
	out := &storage.CachedResult{
		Hits:     make([]storage.CachedHit, 0, len(kept)),
		CachedAt: c.CachedAt,
		ChunkIDs: make([]string, 0, len(kept)),
	}
	for _, h := range c.Hits {
		if _, ok := keep[h.ID]; ok {
			out.Hits = append(out.Hits, h)
			out.ChunkIDs = append(out.ChunkIDs, h.ID)
		}
	}
	return out, blockedACL, blockedRecip
}

// metadataString safely fetches a string-valued key from a Match's
// metadata map. Returns ("", false) when the key is missing or the
// value isn't a string.
func metadataString(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// noopPolicyResolver returns an empty snapshot for every
// (tenant, channel). Used as the default when cmd/api hasn't wired a
// resolver — the platform still enforces the privacy-label
// PolicyFilter against the request-supplied mode.
type noopPolicyResolver struct{}

// Resolve returns an empty snapshot. Leaving EffectiveMode unset
// signals to the handler that no admin-controlled mode is available,
// so it falls back to the request's PrivacyMode (or
// HandlerConfig.DefaultPrivacyMode). Once a real PolicyResolver is
// wired in, its EffectiveMode is the source of truth.
func (noopPolicyResolver) Resolve(_ context.Context, _, _ string) (PolicySnapshot, error) {
	return PolicySnapshot{}, nil
}

// Compile-time assertion that the noop resolver satisfies the port.
var _ PolicyResolver = noopPolicyResolver{}
