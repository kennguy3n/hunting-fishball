package shard_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

func TestEvictionPolicy_ShouldEvict(t *testing.T) {
	t.Parallel()
	policy := shard.EvictionPolicy{
		Tier:                   shard.DeviceTierMid,
		MaxShardSizeMB:         256,
		MinFreeMemoryMB:        512,
		ThermalEvictMultiplier: 0.5,
	}

	cases := []struct {
		name    string
		inputs  shard.EvictionInputs
		want    bool
		wantWhy string
	}{
		{
			name: "unknown tier evicts",
			inputs: shard.EvictionInputs{
				ShardSizeMB:       100,
				DeviceTier:        shard.DeviceTierUnknown,
				AvailableMemoryMB: 4096,
			},
			want:    true,
			wantWhy: "unknown_tier",
		},
		{
			name: "shard too large evicts",
			inputs: shard.EvictionInputs{
				ShardSizeMB:       512,
				DeviceTier:        shard.DeviceTierMid,
				AvailableMemoryMB: 4096,
			},
			want:    true,
			wantWhy: "shard_too_large",
		},
		{
			name: "thermal throttle halves the size limit",
			inputs: shard.EvictionInputs{
				ShardSizeMB:       200, // under 256 normally; over 128 with multiplier
				DeviceTier:        shard.DeviceTierMid,
				AvailableMemoryMB: 4096,
				ThermalThrottled:  true,
			},
			want:    true,
			wantWhy: "shard_too_large",
		},
		{
			name: "memory pressure evicts",
			inputs: shard.EvictionInputs{
				ShardSizeMB:       100,
				DeviceTier:        shard.DeviceTierMid,
				AvailableMemoryMB: 100, // under the 512 MB floor
			},
			want:    true,
			wantWhy: "memory_pressure",
		},
		{
			name: "happy path keeps",
			inputs: shard.EvictionInputs{
				ShardSizeMB:       100,
				DeviceTier:        shard.DeviceTierMid,
				AvailableMemoryMB: 4096,
			},
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := policy.ShouldEvict(tc.inputs)
			if got.Evict != tc.want {
				t.Fatalf("Evict=%v want %v (reason=%q)", got.Evict, tc.want, got.Reason)
			}
			if tc.wantWhy != "" && got.Reason != tc.wantWhy {
				t.Fatalf("Reason=%q want %q", got.Reason, tc.wantWhy)
			}
		})
	}
}

func TestEvictionPolicy_LowTierLargeShard(t *testing.T) {
	t.Parallel()
	// Low-tier baseline: max 64 MB shard, 256 MB free floor.
	low := shard.DefaultEvictionPolicies()[0]
	got := low.ShouldEvict(shard.EvictionInputs{
		ShardSizeMB:       128,
		DeviceTier:        shard.DeviceTierLow,
		AvailableMemoryMB: 4096,
	})
	if !got.Evict || got.Reason != "shard_too_large" {
		t.Fatalf("low+128MB: %+v", got)
	}
}

func TestEvictionPolicyTable_Resolve(t *testing.T) {
	t.Parallel()
	tbl, err := shard.NewEvictionPolicyTable(shard.DefaultEvictionPolicies())
	if err != nil {
		t.Fatalf("NewEvictionPolicyTable: %v", err)
	}
	if tbl.Resolve(shard.DeviceTierHigh).MaxShardSizeMB != 1024 {
		t.Fatalf("high tier policy resolved wrong")
	}
	// Unknown tier collapses to Low.
	if tbl.Resolve(shard.DeviceTierUnknown).Tier != shard.DeviceTierLow {
		t.Fatalf("unknown tier did not collapse to low")
	}
}

func TestEvictionPolicyTable_DuplicateRejected(t *testing.T) {
	t.Parallel()
	dup := []shard.EvictionPolicy{
		{Tier: shard.DeviceTierLow, MaxShardSizeMB: 64},
		{Tier: shard.DeviceTierLow, MaxShardSizeMB: 128},
	}
	if _, err := shard.NewEvictionPolicyTable(dup); err == nil {
		t.Fatalf("expected duplicate-tier error")
	}
	empty := []shard.EvictionPolicy{{Tier: shard.DeviceTierUnknown}}
	if _, err := shard.NewEvictionPolicyTable(empty); err == nil {
		t.Fatalf("expected empty-tier error")
	}
}

func TestEvictionPolicyTable_All_StableOrder(t *testing.T) {
	t.Parallel()
	tbl, err := shard.NewEvictionPolicyTable(shard.DefaultEvictionPolicies())
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	all := tbl.All()
	if len(all) != 3 {
		t.Fatalf("len: %d", len(all))
	}
	if all[0].Tier != shard.DeviceTierLow ||
		all[1].Tier != shard.DeviceTierMid ||
		all[2].Tier != shard.DeviceTierHigh {
		t.Fatalf("order: %v", all)
	}
}

func TestDefaultEvictionPolicies_AllTiers(t *testing.T) {
	t.Parallel()
	got := shard.DefaultEvictionPolicies()
	if len(got) != 3 {
		t.Fatalf("expected 3 tiers, got %d", len(got))
	}
	for _, p := range got {
		if p.MaxShardSizeMB <= 0 || p.MinFreeMemoryMB <= 0 {
			t.Fatalf("tier %s has non-positive limits: %+v", p.Tier, p)
		}
		if p.ThermalEvictMultiplier <= 0 || p.ThermalEvictMultiplier > 1 {
			t.Fatalf("tier %s has invalid multiplier %v", p.Tier, p.ThermalEvictMultiplier)
		}
	}
}
