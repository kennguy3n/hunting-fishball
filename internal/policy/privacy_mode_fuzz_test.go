// Round-12 Task 12 — fuzz target for EffectiveMode.
//
// EffectiveMode picks the strictest of (tenantMode, channelMode).
// Bug-class signals:
//
//  1. Any panic on arbitrary string inputs (the parser must
//     defensively bucket unknowns as PrivacyModeNoAI).
//  2. A non-monotonic output: the output's rank must never be
//     lower (i.e. more permissive) than the strictest input. Any
//     regression here would silently widen policy.
//  3. Output not in the known PrivacyMode enumeration.
package policy_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// rank assigns a numeric rank to a PrivacyMode for the monotonicity
// invariant. Lower index == stricter. Unknowns default to the
// strictest rank so the assertion never silently passes a buggy
// "widening" result. The ordering mirrors orderedModes in
// internal/policy/privacy_mode.go.
func rank(m policy.PrivacyMode) int {
	switch m {
	case policy.PrivacyModeNoAI:
		return 0
	case policy.PrivacyModeLocalOnly:
		return 1
	case policy.PrivacyModeLocalAPI:
		return 2
	case policy.PrivacyModeHybrid:
		return 3
	case policy.PrivacyModeRemote:
		return 4
	default:
		// Treat unknowns as strictest so the monotonicity assertion
		// catches any output whose rank is lower than the strictest
		// input. This collapses unknown-vs-unknown to rank 0.
		return 0
	}
}

// known is the enumeration of valid PrivacyMode values. The fuzz
// target requires EffectiveMode's output to land in this set.
var known = map[policy.PrivacyMode]struct{}{
	policy.PrivacyModeNoAI:      {},
	policy.PrivacyModeLocalOnly: {},
	policy.PrivacyModeLocalAPI:  {},
	policy.PrivacyModeHybrid:    {},
	policy.PrivacyModeRemote:    {},
}

func FuzzEffectiveMode(f *testing.F) {
	seeds := []struct{ a, b string }{
		{"no-ai", "local-only"},
		{"local-api", "remote"},
		{"hybrid", "hybrid"},
		{"", ""},
		{"unknown", "remote"},
		{"\x00", "no-ai"},
	}
	for _, s := range seeds {
		f.Add(s.a, s.b)
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		out := policy.EffectiveMode(policy.PrivacyMode(a), policy.PrivacyMode(b))
		if _, ok := known[out]; !ok {
			t.Fatalf("output %q not in known PrivacyMode enumeration (inputs: %q, %q)", out, a, b)
		}
		// Monotonicity: output must be at-or-stricter than the
		// stricter input. Stricter means lower rank.
		strictRank := rank(policy.PrivacyMode(a))
		if r := rank(policy.PrivacyMode(b)); r < strictRank {
			strictRank = r
		}
		if rank(out) > strictRank {
			t.Fatalf("EffectiveMode widened policy: inputs=(%q,%q) out=%q rank=%d strictest=%d",
				a, b, out, rank(out), strictRank)
		}
	})
}
