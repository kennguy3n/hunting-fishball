package policy

// DeviceTier mirrors the on-device tier classification documented
// in `docs/PROPOSAL.md` §9. Three tiers (Low / Mid / High) plus an
// explicit "unknown" sentinel for a device that hasn't reported.
//
// This mirror is duplicated from `internal/shard.DeviceTier` to
// keep the policy package free of an internal/shard import (which
// would invert the current dependency: shard already imports
// policy via the generation worker). The two enums use identical
// string constants so JSON round-tripping is interchangeable.
type DeviceTier string

const (
	// DeviceTierUnknown is the safe default — the policy treats
	// it as Low for prefer_local decisions.
	DeviceTierUnknown DeviceTier = ""

	// DeviceTierLow has no on-device SLM. The device-first
	// policy never returns prefer_local=true for this tier.
	DeviceTierLow DeviceTier = "low"

	// DeviceTierMid has Bonsai-1.7B int4. prefer_local=true is
	// returned when the privacy mode permits local processing.
	DeviceTierMid DeviceTier = "mid"

	// DeviceTierHigh has Bonsai-1.7B fp16/int8. Same prefer_local
	// rules as Mid; the on-device runtime picks between the two
	// quantizations.
	DeviceTierHigh DeviceTier = "high"
)

// DeviceFirstInputs is the runtime snapshot DeviceFirstDecision
// consumes. The retrieval handler fills it in once per request from
// the request envelope (device_tier + channel local-retrieval flag)
// and the resolved PolicySnapshot.
type DeviceFirstInputs struct {
	// DeviceTier is the requesting device's effective tier.
	DeviceTier DeviceTier

	// AllowLocalRetrieval is the channel's "may we use the
	// on-device shard for this channel" toggle. Defaults to true
	// when the channel policy hasn't been explicitly configured.
	AllowLocalRetrieval bool

	// PrivacyMode is the resolved privacy mode for the
	// (tenant, channel) tuple — already merged through
	// EffectiveMode.
	PrivacyMode PrivacyMode

	// LocalShardVersion is the freshest shard version available
	// for the request scope. 0 means no shard exists yet, in
	// which case prefer_local is always false (there's nothing to
	// prefer).
	LocalShardVersion int64
}

// DeviceFirstDecision is the structured output of
// `Decide`. Surfacing the reason makes the decision auditable —
// operators can see *why* a particular request was routed local or
// remote without re-running the policy.
type DeviceFirstDecision struct {
	// PreferLocal is the hint surfaced to the client. true means
	// "the server believes the on-device shard can serve this
	// query"; the client may still fall back to remote based on
	// its own coverage check.
	PreferLocal bool `json:"prefer_local"`

	// LocalShardVersion echoes the shard version the client
	// should use for the local query. Always populated from the
	// inputs so the client doesn't have to re-fetch the manifest
	// to learn the freshest version.
	LocalShardVersion int64 `json:"local_shard_version"`

	// Reason is a stable, machine-readable label. One of:
	//   "device_tier_too_low"   - device cannot run the SLM
	//   "channel_disallowed"    - channel forbids local retrieval
	//   "privacy_blocks_local"  - privacy mode requires remote
	//   "no_local_shard"        - shard not yet generated
	//   "prefer_local"          - all gates green, local wins
	Reason string `json:"reason,omitempty"`
}

// Decide returns the device-first hint for the supplied inputs.
// "On-device first by default" means the only way a Mid/High
// device on a permissive channel and an allowing privacy mode
// gets a remote-only response is if the shard hasn't been
// generated yet. The decision tree:
//
//  1. Unknown / Low tier  → remote (no SLM eligible).
//  2. Channel disallows   → remote.
//  3. Privacy mode blocks → remote (no-ai or remote-only modes).
//  4. No shard yet        → remote (nothing to prefer).
//  5. Otherwise           → prefer local.
func Decide(in DeviceFirstInputs) DeviceFirstDecision {
	out := DeviceFirstDecision{LocalShardVersion: in.LocalShardVersion}

	switch in.DeviceTier {
	case DeviceTierMid, DeviceTierHigh:
		// proceed
	default:
		out.Reason = "device_tier_too_low"
		return out
	}

	if !in.AllowLocalRetrieval {
		out.Reason = "channel_disallowed"
		return out
	}

	if !permitsLocalRetrieval(in.PrivacyMode) {
		out.Reason = "privacy_blocks_local"
		return out
	}

	if in.LocalShardVersion <= 0 {
		out.Reason = "no_local_shard"
		return out
	}

	out.PreferLocal = true
	out.Reason = "prefer_local"
	return out
}

// permitsLocalRetrieval returns true when the supplied privacy
// mode allows the on-device shard to serve a query. The local
// shard is "on-device" — strictly stricter than local-api — so
// every mode except no-ai (which blocks all retrieval) and the
// "must hit remote" sentinel allows local. We accept
// PrivacyModeRemote as well because remote is the most permissive
// mode and locally-served results are always a strict subset of
// what remote can serve.
func permitsLocalRetrieval(mode PrivacyMode) bool {
	switch mode {
	case PrivacyModeNoAI:
		return false
	case PrivacyModeLocalOnly,
		PrivacyModeLocalAPI,
		PrivacyModeHybrid,
		PrivacyModeRemote:
		return true
	default:
		// Unknown modes fail closed — match the behaviour of
		// EffectiveMode, which treats unknown as the strictest.
		return false
	}
}
