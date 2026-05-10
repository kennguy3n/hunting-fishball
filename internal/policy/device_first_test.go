package policy_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestDecide_PreferLocal_HappyPath(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierHigh,
		AllowLocalRetrieval: true,
		PrivacyMode:         policy.PrivacyModeHybrid,
		LocalShardVersion:   7,
	})
	if !got.PreferLocal {
		t.Fatalf("expected prefer_local=true, got %+v", got)
	}
	if got.Reason != "prefer_local" {
		t.Fatalf("reason=%q", got.Reason)
	}
	if got.LocalShardVersion != 7 {
		t.Fatalf("version=%d", got.LocalShardVersion)
	}
}

func TestDecide_PreferLocal_MidTier(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierMid,
		AllowLocalRetrieval: true,
		PrivacyMode:         policy.PrivacyModeLocalOnly,
		LocalShardVersion:   3,
	})
	if !got.PreferLocal {
		t.Fatalf("expected prefer_local=true, got %+v", got)
	}
}

func TestDecide_LowTier_RoutesRemote(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierLow,
		AllowLocalRetrieval: true,
		PrivacyMode:         policy.PrivacyModeHybrid,
		LocalShardVersion:   7,
	})
	if got.PreferLocal {
		t.Fatalf("low tier should not prefer local")
	}
	if got.Reason != "device_tier_too_low" {
		t.Fatalf("reason=%q", got.Reason)
	}
}

func TestDecide_UnknownTier_RoutesRemote(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierUnknown,
		AllowLocalRetrieval: true,
		PrivacyMode:         policy.PrivacyModeHybrid,
		LocalShardVersion:   7,
	})
	if got.PreferLocal {
		t.Fatalf("unknown tier should not prefer local")
	}
	if got.Reason != "device_tier_too_low" {
		t.Fatalf("reason=%q", got.Reason)
	}
}

func TestDecide_ChannelDisallowed_RoutesRemote(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierHigh,
		AllowLocalRetrieval: false,
		PrivacyMode:         policy.PrivacyModeHybrid,
		LocalShardVersion:   7,
	})
	if got.PreferLocal {
		t.Fatalf("channel-disallowed should route remote")
	}
	if got.Reason != "channel_disallowed" {
		t.Fatalf("reason=%q", got.Reason)
	}
}

func TestDecide_NoAIMode_RoutesRemote(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierHigh,
		AllowLocalRetrieval: true,
		PrivacyMode:         policy.PrivacyModeNoAI,
		LocalShardVersion:   7,
	})
	if got.PreferLocal {
		t.Fatalf("no-ai mode should never prefer local")
	}
	if got.Reason != "privacy_blocks_local" {
		t.Fatalf("reason=%q", got.Reason)
	}
}

func TestDecide_NoShardYet_RoutesRemote(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierHigh,
		AllowLocalRetrieval: true,
		PrivacyMode:         policy.PrivacyModeHybrid,
		LocalShardVersion:   0,
	})
	if got.PreferLocal {
		t.Fatalf("no shard should not prefer local")
	}
	if got.Reason != "no_local_shard" {
		t.Fatalf("reason=%q", got.Reason)
	}
}

func TestDecide_AllAllowingModes(t *testing.T) {
	t.Parallel()
	allowed := []policy.PrivacyMode{
		policy.PrivacyModeLocalOnly,
		policy.PrivacyModeLocalAPI,
		policy.PrivacyModeHybrid,
		policy.PrivacyModeRemote,
	}
	for _, m := range allowed {
		m := m
		t.Run(string(m), func(t *testing.T) {
			t.Parallel()
			got := policy.Decide(policy.DeviceFirstInputs{
				DeviceTier:          policy.DeviceTierHigh,
				AllowLocalRetrieval: true,
				PrivacyMode:         m,
				LocalShardVersion:   1,
			})
			if !got.PreferLocal {
				t.Fatalf("mode %s should prefer local, got %+v", m, got)
			}
		})
	}
}

// TestDecide_FullMatrix exercises every combination of the five
// privacy modes × three device tiers documented in
// docs/PROPOSAL.md §9, asserting both PreferLocal and Reason. The
// matrix is the contract device-tier classification has with the
// retrieval handler — extending it without updating this test
// would silently change client behaviour.
func TestDecide_FullMatrix(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name           string
		tier           policy.DeviceTier
		eligible       bool
		expectedReason string
	}{
		{"low", policy.DeviceTierLow, false, "device_tier_too_low"},
		{"mid", policy.DeviceTierMid, true, ""},
		{"high", policy.DeviceTierHigh, true, ""},
	}
	modes := []struct {
		name   string
		mode   policy.PrivacyMode
		permit bool // true if mode permits local retrieval
	}{
		{"no_ai", policy.PrivacyModeNoAI, false},
		{"local_only", policy.PrivacyModeLocalOnly, true},
		{"local_api", policy.PrivacyModeLocalAPI, true},
		{"hybrid", policy.PrivacyModeHybrid, true},
		{"remote", policy.PrivacyModeRemote, true},
	}
	for _, tier := range tiers {
		tier := tier
		for _, m := range modes {
			m := m
			t.Run(tier.name+"+"+m.name, func(t *testing.T) {
				t.Parallel()
				got := policy.Decide(policy.DeviceFirstInputs{
					DeviceTier:          tier.tier,
					AllowLocalRetrieval: true,
					PrivacyMode:         m.mode,
					LocalShardVersion:   1,
				})
				switch {
				case !tier.eligible:
					if got.PreferLocal {
						t.Fatalf("ineligible tier %s should not prefer local", tier.name)
					}
					if got.Reason != "device_tier_too_low" {
						t.Fatalf("reason=%q want device_tier_too_low", got.Reason)
					}
				case !m.permit:
					if got.PreferLocal {
						t.Fatalf("blocking mode %s should not prefer local", m.name)
					}
					if got.Reason != "privacy_blocks_local" {
						t.Fatalf("reason=%q want privacy_blocks_local", got.Reason)
					}
				default:
					if !got.PreferLocal {
						t.Fatalf("eligible+permitted should prefer local: tier=%s mode=%s got=%+v", tier.name, m.name, got)
					}
					if got.Reason != "prefer_local" {
						t.Fatalf("reason=%q want prefer_local", got.Reason)
					}
				}
			})
		}
	}
}

// TestDecide_ChannelDisallowsTakesPriorityOverPrivacy asserts the
// ordering documented in the Decide doc-string: the channel-level
// deny short-circuits before the privacy gate runs. Without this
// guarantee a tenant could observe `privacy_blocks_local` for a
// privacy mode that, in another context, would actually permit
// local — masking an authoritative deny behind a generic message.
func TestDecide_ChannelDisallowsTakesPriorityOverPrivacy(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierHigh,
		AllowLocalRetrieval: false,
		PrivacyMode:         policy.PrivacyModeNoAI,
		LocalShardVersion:   7,
	})
	if got.Reason != "channel_disallowed" {
		t.Fatalf("reason=%q want channel_disallowed (precedence)", got.Reason)
	}
}

// TestDecide_NilSnapshotEquivalent asserts an empty / zero-value
// inputs struct fails closed with no_shard / device_tier_too_low —
// the production caller passes the live snapshot into
// DeviceFirstInputs and we want a missing snapshot to manifest as a
// remote-only decision instead of panicking.
func TestDecide_NilSnapshotEquivalent(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{})
	if got.PreferLocal {
		t.Fatalf("zero-value inputs should not prefer local: %+v", got)
	}
	if got.Reason == "" {
		t.Fatalf("expected a reason; got empty")
	}
}

func TestDecide_UnknownPrivacyMode_FailsClosed(t *testing.T) {
	t.Parallel()
	got := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTierHigh,
		AllowLocalRetrieval: true,
		PrivacyMode:         policy.PrivacyMode("definitely-not-a-real-mode"),
		LocalShardVersion:   7,
	})
	if got.PreferLocal {
		t.Fatalf("unknown mode should fail closed, got %+v", got)
	}
	if got.Reason != "privacy_blocks_local" {
		t.Fatalf("reason=%q", got.Reason)
	}
}
