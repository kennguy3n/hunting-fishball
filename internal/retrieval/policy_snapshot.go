package retrieval

import (
	"context"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// PolicySnapshot is the per-(tenant, channel) policy carrier the
// retrieval handler reads before fan-out. The canonical type lives
// in internal/policy so the simulator (Phase 4) can build, diff,
// and promote snapshots without importing retrieval. Retrieval
// re-exports the type via this alias to keep existing call sites
// compiling unchanged.
type PolicySnapshot = policy.PolicySnapshot

// PolicyResolver mirrors policy.PolicyResolver. The alias lets the
// retrieval handler keep its existing port name while sharing the
// underlying interface with the policy simulator.
type PolicyResolver = policy.PolicyResolver

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
		path, _ := matchPath(m)
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

// filterCachedByPrivacyMode re-applies the privacy-label PolicyFilter
// to a CachedResult on the cache-hit path using the *resolved*
// effective privacy mode. The retrieval cache TTL is minutes-long, so
// when an admin tightens the tenant/channel mode between cache write
// and read, cached chunks whose PrivacyLabel exceeds the new mode
// MUST be dropped — otherwise the more permissive mode keeps leaking
// content until the TTL expires.
//
// Returns the (possibly trimmed) CachedResult and the count blocked
// by privacy mode. nil PolicyFilter is a pass-through (used by call
// sites where the filter is intentionally disabled).
func filterCachedByPrivacyMode(c *storage.CachedResult, pf *PolicyFilter, privacyMode string) (*storage.CachedResult, int) {
	if c == nil || len(c.Hits) == 0 || pf == nil {
		return c, 0
	}
	shadows := make([]*Match, len(c.Hits))
	for i, h := range c.Hits {
		shadows[i] = &Match{ID: h.ID, PrivacyLabel: h.PrivacyLabel}
	}
	pres := pf.Apply(shadows, privacyMode)
	if pres.BlockedCount == 0 {
		return c, 0
	}
	keep := make(map[string]struct{}, len(pres.Allowed))
	for _, m := range pres.Allowed {
		keep[m.ID] = struct{}{}
	}
	out := &storage.CachedResult{
		Hits:     make([]storage.CachedHit, 0, len(pres.Allowed)),
		CachedAt: c.CachedAt,
		ChunkIDs: make([]string, 0, len(pres.Allowed)),
	}
	for _, h := range c.Hits {
		if _, ok := keep[h.ID]; ok {
			out.Hits = append(out.Hits, h)
			out.ChunkIDs = append(out.ChunkIDs, h.ID)
		}
	}
	return out, pres.BlockedCount
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
			URI:      h.URI,
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
