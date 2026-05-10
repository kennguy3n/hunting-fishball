package shard

import (
	"errors"
	"sort"
)

// DeviceTier mirrors the on-device tier classification documented
// in `docs/PROPOSAL.md` §9. Three tiers (Low / Mid / High) plus an
// explicit "unknown" sentinel for a device that hasn't reported.
type DeviceTier string

const (
	// DeviceTierUnknown is the safe default — treat the device as
	// the strictest tier (Low) for eviction decisions.
	DeviceTierUnknown DeviceTier = ""

	// DeviceTierLow corresponds to RAM < 4 GB / CPU only. The
	// runtime never holds an SLM resident; large shards are
	// evicted aggressively.
	DeviceTierLow DeviceTier = "low"

	// DeviceTierMid corresponds to 4–8 GB RAM / optional NPU. The
	// runtime can hold Bonsai-1.7B int4 plus a small shard; a
	// large shard forces the SLM to evict the shard.
	DeviceTierMid DeviceTier = "mid"

	// DeviceTierHigh corresponds to ≥ 8 GB RAM / NPU available.
	// The runtime can hold Bonsai-1.7B fp16/int8 plus most
	// shards.
	DeviceTierHigh DeviceTier = "high"
)

// EvictionPolicy holds the per-tier thresholds that drive the
// `ShouldEvict` decision. The policy is part of the model catalog
// (`internal/models.ModelCatalog.EvictionConfig`) so platform
// updates can ship new thresholds without a client release.
//
// All sizes are in megabytes. AvailableMemoryMB is the runtime's
// snapshot of free memory at decision time; the policy compares
// against `MaxShardSizeMB` and `MinFreeMemoryMB` (whichever is
// stricter wins). ThermalEvictMultiplier scales the shard-size
// limit down under thermal pressure — e.g. 0.5 means "evict when
// the shard is half the normal limit if the device is hot".
type EvictionPolicy struct {
	Tier                   DeviceTier `json:"tier"`
	MaxShardSizeMB         int        `json:"max_shard_size_mb"`
	MinFreeMemoryMB        int        `json:"min_free_memory_mb"`
	ThermalEvictMultiplier float64    `json:"thermal_evict_multiplier"`
}

// EvictionInputs is the runtime snapshot ShouldEvict consumes. The
// platform binding fills it in once per decision.
type EvictionInputs struct {
	// ShardSizeMB is the on-disk + in-memory footprint of the
	// shard the runtime is considering retaining.
	ShardSizeMB int

	// DeviceTier is the runtime's effective tier (already
	// downgraded under thermal / battery pressure if applicable).
	DeviceTier DeviceTier

	// AvailableMemoryMB is the runtime's snapshot of free RAM.
	AvailableMemoryMB int

	// ThermalThrottled is true when the OS reports a thermal
	// pressure state above "nominal". Forces the policy to apply
	// ThermalEvictMultiplier on top of the size limit.
	ThermalThrottled bool
}

// EvictionDecision carries both the boolean answer and the reason.
// Surfacing the reason lets the platform binding emit a structured
// log + telemetry event so operators can debug why a particular
// device is thrashing the cache.
type EvictionDecision struct {
	Evict  bool   `json:"evict"`
	Reason string `json:"reason,omitempty"`
}

// ShouldEvict applies an EvictionPolicy against an EvictionInputs
// snapshot and returns an EvictionDecision. The decision is
// deterministic: the same inputs always produce the same output,
// so two devices on the same tier converge to the same eviction
// behaviour.
//
// Decision order (first hit wins):
//
//  1. Unknown tier → evict (fail closed; treat as Low).
//  2. Shard size exceeds tier maximum (with thermal multiplier
//     applied when the device is throttled) → evict.
//  3. Available memory below tier minimum → evict to free RAM.
//  4. Otherwise → keep.
func (p EvictionPolicy) ShouldEvict(inputs EvictionInputs) EvictionDecision {
	if inputs.DeviceTier == DeviceTierUnknown {
		return EvictionDecision{Evict: true, Reason: "unknown_tier"}
	}
	maxShard := p.MaxShardSizeMB
	if inputs.ThermalThrottled && p.ThermalEvictMultiplier > 0 {
		maxShard = int(float64(p.MaxShardSizeMB) * p.ThermalEvictMultiplier)
	}
	if maxShard > 0 && inputs.ShardSizeMB > maxShard {
		return EvictionDecision{Evict: true, Reason: "shard_too_large"}
	}
	if p.MinFreeMemoryMB > 0 && inputs.AvailableMemoryMB < p.MinFreeMemoryMB {
		return EvictionDecision{Evict: true, Reason: "memory_pressure"}
	}
	return EvictionDecision{Evict: false}
}

// EvictionPolicyTable holds one EvictionPolicy per device tier.
// `Resolve` returns the policy for a tier (with a sane default for
// unknown tiers).
type EvictionPolicyTable struct {
	policies map[DeviceTier]EvictionPolicy
}

// NewEvictionPolicyTable builds a table from the supplied
// per-tier policies. Returns an error if any policy's Tier is
// duplicated or empty.
func NewEvictionPolicyTable(policies []EvictionPolicy) (*EvictionPolicyTable, error) {
	t := &EvictionPolicyTable{policies: make(map[DeviceTier]EvictionPolicy, len(policies))}
	for _, p := range policies {
		if p.Tier == DeviceTierUnknown {
			return nil, errors.New("eviction: tier required")
		}
		if _, dup := t.policies[p.Tier]; dup {
			return nil, errors.New("eviction: duplicate tier " + string(p.Tier))
		}
		t.policies[p.Tier] = p
	}
	return t, nil
}

// Resolve returns the policy for tier. An unknown tier resolves to
// the strictest registered policy (Low if registered) so a device
// that hasn't reported gets the safest behaviour.
func (t *EvictionPolicyTable) Resolve(tier DeviceTier) EvictionPolicy {
	if p, ok := t.policies[tier]; ok {
		return p
	}
	if p, ok := t.policies[DeviceTierLow]; ok {
		return p
	}
	// Last resort: a maximally-strict synthesised policy.
	return EvictionPolicy{
		Tier:                   DeviceTierLow,
		MaxShardSizeMB:         64,
		MinFreeMemoryMB:        256,
		ThermalEvictMultiplier: 0.5,
	}
}

// All returns every registered policy ordered by tier (low → high)
// so JSON encodings of the catalog are stable.
func (t *EvictionPolicyTable) All() []EvictionPolicy {
	out := make([]EvictionPolicy, 0, len(t.policies))
	for _, p := range t.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return TierRank(out[i].Tier) < TierRank(out[j].Tier)
	})
	return out
}

// TierRank orders the tiers so callers can compare them ordinally.
// Low is the strictest (rank 0), High is the most permissive
// (rank 2). Unknown returns -1 so it sorts before every real tier
// and never satisfies a `>=` comparison against a real tier.
func TierRank(t DeviceTier) int {
	switch t {
	case DeviceTierLow:
		return 0
	case DeviceTierMid:
		return 1
	case DeviceTierHigh:
		return 2
	default:
		return -1
	}
}

// DefaultEvictionPolicies are the three baseline policies derived
// from `docs/PROPOSAL.md` §9. The numbers are deliberately
// conservative — the model catalog can override them remotely.
//
// Low: very aggressive (evict any shard > 64 MB, keep ≥ 256 MB free).
// Mid: balanced (evict shard > 256 MB, keep ≥ 512 MB free).
// High: permissive (evict shard > 1024 MB, keep ≥ 1024 MB free).
//
// ThermalEvictMultiplier=0.5 across the board: under thermal
// pressure the shard-size limit halves (so a 1024 MB shard on a
// hot High-tier device gets evicted at 512 MB).
func DefaultEvictionPolicies() []EvictionPolicy {
	return []EvictionPolicy{
		{Tier: DeviceTierLow, MaxShardSizeMB: 64, MinFreeMemoryMB: 256, ThermalEvictMultiplier: 0.5},
		{Tier: DeviceTierMid, MaxShardSizeMB: 256, MinFreeMemoryMB: 512, ThermalEvictMultiplier: 0.5},
		{Tier: DeviceTierHigh, MaxShardSizeMB: 1024, MinFreeMemoryMB: 1024, ThermalEvictMultiplier: 0.5},
	}
}
