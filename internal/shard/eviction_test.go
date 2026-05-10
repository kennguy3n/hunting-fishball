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
			want:    false,
			wantWhy: "keep",
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

// TestDefaultEvictionPolicies_ExactValues pins the three default
// thresholds documented in `docs/PROPOSAL.md` §9. The model
// catalog may override these at runtime, but the in-binary
// fallback must match the spec — clients that ship without
// downloading the catalog (offline-first) rely on these numbers.
func TestDefaultEvictionPolicies_ExactValues(t *testing.T) {
	t.Parallel()
	got := shard.DefaultEvictionPolicies()
	want := []shard.EvictionPolicy{
		{Tier: shard.DeviceTierLow, MaxShardSizeMB: 64, MinFreeMemoryMB: 256, ThermalEvictMultiplier: 0.5},
		{Tier: shard.DeviceTierMid, MaxShardSizeMB: 256, MinFreeMemoryMB: 512, ThermalEvictMultiplier: 0.5},
		{Tier: shard.DeviceTierHigh, MaxShardSizeMB: 1024, MinFreeMemoryMB: 1024, ThermalEvictMultiplier: 0.5},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d policies want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("policy[%d]=%+v want %+v", i, got[i], want[i])
		}
	}
}

// TestShouldEvict_FullMatrix walks every combination of the three
// device tiers (Low/Mid/High) × thermal state (normal/throttled) ×
// memory state (ok/low) × shard state (ok/large) — 24 combos. The
// truth-table is the contract the on-device runtime relies on; a
// regression here would alter shard caching behaviour silently.
func TestShouldEvict_FullMatrix(t *testing.T) {
	t.Parallel()
	policies := map[shard.DeviceTier]shard.EvictionPolicy{
		shard.DeviceTierLow:  {Tier: shard.DeviceTierLow, MaxShardSizeMB: 64, MinFreeMemoryMB: 256, ThermalEvictMultiplier: 0.5},
		shard.DeviceTierMid:  {Tier: shard.DeviceTierMid, MaxShardSizeMB: 256, MinFreeMemoryMB: 512, ThermalEvictMultiplier: 0.5},
		shard.DeviceTierHigh: {Tier: shard.DeviceTierHigh, MaxShardSizeMB: 1024, MinFreeMemoryMB: 1024, ThermalEvictMultiplier: 0.5},
	}

	cases := []struct {
		tier    shard.DeviceTier
		thermal bool
		memOK   bool
		shardOK bool
		want    bool
		wantWhy string
	}{
		// Low tier
		{shard.DeviceTierLow, false, true, true, false, "keep"},
		{shard.DeviceTierLow, false, true, false, true, "shard_too_large"},
		{shard.DeviceTierLow, false, false, true, true, "memory_pressure"},
		{shard.DeviceTierLow, false, false, false, true, "shard_too_large"},
		{shard.DeviceTierLow, true, true, true, false, "keep"},
		{shard.DeviceTierLow, true, true, false, true, "shard_too_large"},
		{shard.DeviceTierLow, true, false, true, true, "memory_pressure"},
		{shard.DeviceTierLow, true, false, false, true, "shard_too_large"},
		// Mid tier
		{shard.DeviceTierMid, false, true, true, false, "keep"},
		{shard.DeviceTierMid, false, true, false, true, "shard_too_large"},
		{shard.DeviceTierMid, false, false, true, true, "memory_pressure"},
		{shard.DeviceTierMid, false, false, false, true, "shard_too_large"},
		{shard.DeviceTierMid, true, true, true, false, "keep"},
		{shard.DeviceTierMid, true, true, false, true, "shard_too_large"},
		{shard.DeviceTierMid, true, false, true, true, "memory_pressure"},
		{shard.DeviceTierMid, true, false, false, true, "shard_too_large"},
		// High tier
		{shard.DeviceTierHigh, false, true, true, false, "keep"},
		{shard.DeviceTierHigh, false, true, false, true, "shard_too_large"},
		{shard.DeviceTierHigh, false, false, true, true, "memory_pressure"},
		{shard.DeviceTierHigh, false, false, false, true, "shard_too_large"},
		{shard.DeviceTierHigh, true, true, true, false, "keep"},
		{shard.DeviceTierHigh, true, true, false, true, "shard_too_large"},
		{shard.DeviceTierHigh, true, false, true, true, "memory_pressure"},
		{shard.DeviceTierHigh, true, false, false, true, "shard_too_large"},
	}

	if len(cases) != 24 {
		t.Fatalf("expected 24 combinations, got %d", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		policy := policies[tc.tier]
		// Pick concrete sizes that fall on the correct side of the
		// per-tier thresholds. shardOK=true means well under the
		// limit; memOK=true means well over the floor.
		shardSize := policy.MaxShardSizeMB / 2
		if !tc.shardOK {
			shardSize = policy.MaxShardSizeMB * 2
		}
		mem := policy.MinFreeMemoryMB * 2
		if !tc.memOK {
			mem = policy.MinFreeMemoryMB / 2
		}

		t.Run(string(tc.tier)+"/"+thermalLabel(tc.thermal)+"/"+memLabel(tc.memOK)+"/"+shardLabel(tc.shardOK), func(t *testing.T) {
			t.Parallel()
			got := policy.ShouldEvict(shard.EvictionInputs{
				ShardSizeMB:       shardSize,
				DeviceTier:        tc.tier,
				AvailableMemoryMB: mem,
				ThermalThrottled:  tc.thermal,
			})
			if got.Evict != tc.want {
				t.Fatalf("evict=%v want %v (decision=%+v)", got.Evict, tc.want, got)
			}
			if got.Reason != tc.wantWhy {
				t.Fatalf("reason=%q want %q", got.Reason, tc.wantWhy)
			}
		})
	}
}

func thermalLabel(b bool) string {
	if b {
		return "thermal"
	}
	return "normal"
}

func memLabel(ok bool) string {
	if ok {
		return "mem_ok"
	}
	return "mem_low"
}

func shardLabel(ok bool) string {
	if ok {
		return "shard_ok"
	}
	return "shard_large"
}

// TestShouldEvict_ThermalShrinksLimit asserts the multiplier
// halves the shard-size limit. Without thermal throttling a
// 200 MB shard fits inside the High tier's 1024 MB limit; with
// thermal=true and a 0.5 multiplier the effective limit is 512 MB
// — still room — so we use a 600 MB shard to land inside the
// thermal-only eviction band.
func TestShouldEvict_ThermalShrinksLimit(t *testing.T) {
	t.Parallel()
	p := shard.EvictionPolicy{Tier: shard.DeviceTierHigh, MaxShardSizeMB: 1024, MinFreeMemoryMB: 1024, ThermalEvictMultiplier: 0.5}
	// 600 MB fits the normal limit but not the thermal-shrunk one.
	got := p.ShouldEvict(shard.EvictionInputs{ShardSizeMB: 600, DeviceTier: shard.DeviceTierHigh, AvailableMemoryMB: 4096, ThermalThrottled: false})
	if got.Evict {
		t.Errorf("normal: should keep 600 MB shard, got %+v", got)
	}
	got = p.ShouldEvict(shard.EvictionInputs{ShardSizeMB: 600, DeviceTier: shard.DeviceTierHigh, AvailableMemoryMB: 4096, ThermalThrottled: true})
	if !got.Evict || got.Reason != "shard_too_large" {
		t.Errorf("thermal: should evict 600 MB shard, got %+v", got)
	}
}
