// Package benchmark hosts the Phase 8 / Task 18 Bonsai-1.7B
// benchmark *contract*. The contract describes the performance
// envelope each device tier MUST hit; the actual on-device runs
// happen in the kennguy3n/knowledge and kennguy3n/llama.cpp
// repositories, which import these structs and assert each
// platform's measurements stay within the envelope before they
// publish a release.
//
// Keeping the contract here (not in the on-device repos) means
// the same source of truth ships in the server release, so when
// the server team tunes shard sizing or eviction policies, the
// contract values can be updated alongside the model catalog
// in a single PR.
package benchmark

import (
	"strings"
	"testing"
)

// TierBenchmark is the per-tier performance envelope. The values
// are floors / ceilings the on-device runtime must satisfy:
//
//   - MinTokensPerSec is a *floor* — the runtime must achieve at
//     least this throughput on the tier's reference device.
//   - MaxFirstTokenLatencyMs is a *ceiling* — TTFT must not exceed
//     this on the reference device.
//   - MaxMemoryMB is a *ceiling* — resident set size during a
//     2k-token decode must stay under this.
//
// The reference devices are documented in PROPOSAL.md §9.
type TierBenchmark struct {
	Tier                   string
	MinTokensPerSec        float64
	MaxFirstTokenLatencyMs int64
	MaxMemoryMB            int
}

// BonsaiContract is the immutable Phase 8 contract. Updating these
// numbers is a deliberate, reviewed change; the on-device repos
// pin to a hash of this list so a silent tightening here can't
// accidentally fail their builds.
//
// The numbers below are the targets named in PROPOSAL.md §9 ("Bonsai
// performance envelope per tier"). They are NOT measurements; they
// are the envelope each measurement must clear.
var BonsaiContract = []TierBenchmark{
	{
		Tier:                   "low",
		MinTokensPerSec:        4.0,
		MaxFirstTokenLatencyMs: 1500,
		MaxMemoryMB:            900,
	},
	{
		Tier:                   "mid",
		MinTokensPerSec:        12.0,
		MaxFirstTokenLatencyMs: 600,
		MaxMemoryMB:            1400,
	},
	{
		Tier:                   "high",
		MinTokensPerSec:        25.0,
		MaxFirstTokenLatencyMs: 300,
		MaxMemoryMB:            3600,
	},
}

// SatisfiesContract returns the first tier-specific failure when
// (tokensPerSec, firstTokenLatencyMs, memoryMB) does not satisfy
// the contract for tier. The empty string means the tuple cleared
// the contract. Unknown tiers fail closed.
func SatisfiesContract(tier string, tokensPerSec float64, firstTokenLatencyMs int64, memoryMB int) string {
	for _, b := range BonsaiContract {
		if b.Tier != tier {
			continue
		}
		if tokensPerSec < b.MinTokensPerSec {
			return "tokens_per_sec_below_floor"
		}
		if firstTokenLatencyMs > b.MaxFirstTokenLatencyMs {
			return "first_token_latency_above_ceiling"
		}
		if memoryMB > b.MaxMemoryMB {
			return "memory_above_ceiling"
		}
		return ""
	}
	return "unknown_tier"
}

// TestBonsaiContract_AllTiersPresent guards against a future edit
// that drops one of the three tiers. The on-device repos rely on
// the slice ordering and presence to build per-platform CI
// matrices.
func TestBonsaiContract_AllTiersPresent(t *testing.T) {
	t.Parallel()
	want := []string{"low", "mid", "high"}
	if len(BonsaiContract) != len(want) {
		t.Fatalf("BonsaiContract has %d entries, want %d", len(BonsaiContract), len(want))
	}
	for i, w := range want {
		if BonsaiContract[i].Tier != w {
			t.Fatalf("BonsaiContract[%d].Tier = %q, want %q", i, BonsaiContract[i].Tier, w)
		}
	}
}

// TestBonsaiContract_Monotonic asserts each successively higher
// tier raises the throughput floor and lowers (or holds) the TTFT
// ceiling — a Mid device should never have a lower throughput
// floor than a Low device, otherwise the contract is malformed.
func TestBonsaiContract_Monotonic(t *testing.T) {
	t.Parallel()
	for i := 1; i < len(BonsaiContract); i++ {
		prev := BonsaiContract[i-1]
		curr := BonsaiContract[i]
		if curr.MinTokensPerSec <= prev.MinTokensPerSec {
			t.Fatalf("tier %s tokens floor (%v) <= tier %s (%v)", curr.Tier, curr.MinTokensPerSec, prev.Tier, prev.MinTokensPerSec)
		}
		if curr.MaxFirstTokenLatencyMs >= prev.MaxFirstTokenLatencyMs {
			t.Fatalf("tier %s TTFT ceiling (%d) >= tier %s (%d)", curr.Tier, curr.MaxFirstTokenLatencyMs, prev.Tier, prev.MaxFirstTokenLatencyMs)
		}
	}
}

// TestSatisfiesContract_HappyPath ensures the helper correctly
// validates a tuple that clears the envelope.
func TestSatisfiesContract_HappyPath(t *testing.T) {
	t.Parallel()
	if got := SatisfiesContract("mid", 12.5, 500, 1200); got != "" {
		t.Fatalf("expected pass, got reason=%q", got)
	}
	if got := SatisfiesContract("high", 30.0, 250, 3500); got != "" {
		t.Fatalf("expected pass, got reason=%q", got)
	}
}

// TestSatisfiesContract_Failures spot-checks each failure label.
func TestSatisfiesContract_Failures(t *testing.T) {
	t.Parallel()
	cases := map[string][4]any{
		"tokens_per_sec_below_floor":        {"mid", 5.0, int64(500), 1200},
		"first_token_latency_above_ceiling": {"mid", 12.5, int64(900), 1200},
		"memory_above_ceiling":              {"mid", 12.5, int64(500), 9000},
		"unknown_tier":                      {"ultra", 50.0, int64(100), 100},
	}
	for wantReason, tup := range cases {
		tier := tup[0].(string)
		tps := tup[1].(float64)
		ttft := tup[2].(int64)
		mem := tup[3].(int)
		got := SatisfiesContract(tier, tps, ttft, mem)
		if got != wantReason {
			t.Fatalf("tier=%s tps=%v ttft=%v mem=%v: got %q want %q",
				tier, tps, ttft, mem, got, wantReason)
		}
	}
}

// TestBonsaiContract_TiersLowerCase guards against a casing typo
// that would silently mismatch internal/shard.DeviceTier values
// (which are also lower-case strings).
func TestBonsaiContract_TiersLowerCase(t *testing.T) {
	t.Parallel()
	for _, b := range BonsaiContract {
		if b.Tier != strings.ToLower(b.Tier) {
			t.Fatalf("tier %q is not lower-case", b.Tier)
		}
	}
}
