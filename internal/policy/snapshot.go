package policy

import "context"

// PolicySnapshot captures every policy decision the retrieval handler
// (and the simulator) needs for one (tenant, channel) at request
// time. The resolver loads it from Postgres (or an in-process cache);
// callers treat it as immutable.
//
// The snapshot lives in the policy package — rather than retrieval —
// so it can be passed by value into the simulator's what-if engine
// without creating an import cycle (retrieval already depends on
// policy). The retrieval package re-exports it as a type alias so
// existing call sites don't need to import the policy package
// directly.
type PolicySnapshot struct {
	// EffectiveMode is the strict-of (tenantMode, channelMode). The
	// retrieval handler passes its string form to the existing
	// privacy-label PolicyFilter.
	EffectiveMode PrivacyMode

	// ACL is the per-(tenant, channel) allow/deny list. nil → no
	// ACL installed (default-allow).
	ACL *AllowDenyList

	// Recipient is the per-(tenant, channel) recipient policy. nil
	// → default-allow for every skill.
	Recipient *RecipientPolicy
}

// Clone returns a deep copy of the snapshot. Used by the simulator
// to materialise a copy-on-write view when running what-if retrievals
// — mutating the live resolver's snapshot would otherwise leak into
// concurrent retrieval calls. Both AllowDenyList.Rules and
// RecipientPolicy.Rules are slice-copied so a caller appending a rule
// to the clone does not mutate the live snapshot's underlying array.
func (s PolicySnapshot) Clone() PolicySnapshot {
	out := PolicySnapshot{EffectiveMode: s.EffectiveMode}
	if s.ACL != nil {
		acl := *s.ACL
		acl.Rules = append([]ACLRule(nil), s.ACL.Rules...)
		out.ACL = &acl
	}
	if s.Recipient != nil {
		rp := *s.Recipient
		rp.Rules = append([]RecipientRule(nil), s.Recipient.Rules...)
		out.Recipient = &rp
	}
	return out
}

// PolicyResolver resolves a PolicySnapshot for a (tenant, channel)
// pair. Implementations live in cmd/api/main.go (Postgres-backed) or
// in tests (in-memory). When the resolver returns an error, the
// retrieval handler MUST fail closed — return zero hits with
// PrivacyMode set to NoAI — rather than silently widening the policy.
type PolicyResolver interface {
	Resolve(ctx context.Context, tenantID, channelID string) (PolicySnapshot, error)
}

// ResolverFunc adapts a plain function to PolicyResolver. Convenient
// for tests and inline wrappers (e.g. the simulator's draft override).
type ResolverFunc func(ctx context.Context, tenantID, channelID string) (PolicySnapshot, error)

// Resolve implements PolicyResolver.
func (f ResolverFunc) Resolve(ctx context.Context, tenantID, channelID string) (PolicySnapshot, error) {
	return f(ctx, tenantID, channelID)
}
