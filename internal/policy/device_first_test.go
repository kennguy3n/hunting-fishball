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
