// Package models holds the static model catalog the on-device tier
// reads to discover which Bonsai-1.7B build is eligible for each
// device tier. The catalog is a small, hand-curated list — the
// runtime *signature verification* of the catalog (per
// `docs/PROPOSAL.md` §9) lands separately in
// `kennguy3n/slm-rich-media`; this package only owns the shape.
//
// The catalog is exposed to clients via
// `GET /v1/models/catalog`. The on-device runtime fetches the
// catalog, picks the model whose tier floor matches the device's
// effective tier, and downloads the GGUF + verifies the signature.
package models

import (
	"errors"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

// ModelEntry describes a single distributable model build. One
// row per (model, quantization, tier-floor) tuple.
type ModelEntry struct {
	// ID is the catalog-stable identifier — e.g.
	// "bonsai-1.7b-q4_0". Clients pin against this id so tier
	// re-tunings don't accidentally swap a model out from under
	// them.
	ID string `json:"id"`

	// Name is the human-readable model name surfaced in the UI.
	Name string `json:"name"`

	// Family groups related quantizations (e.g. all
	// "bonsai-1.7b" entries share Family="bonsai-1.7b").
	Family string `json:"family"`

	// TierFloor is the minimum DeviceTier on which this build is
	// eligible. A device on a stricter tier must use a smaller
	// quantization or skip the model entirely.
	TierFloor shard.DeviceTier `json:"tier_floor"`

	// Quantization names the GGUF quantization (e.g. "q4_0",
	// "q8_0", "fp16"). Mirrors llama.cpp's quantization names.
	Quantization string `json:"quantization"`

	// SizeMB is the on-disk + in-memory footprint of the model
	// build. Used by the eviction policy to decide whether the
	// device has headroom to keep it resident.
	SizeMB int `json:"size_mb"`

	// MaxContextTokens is the model's context-window limit. The
	// runtime caps the on-device retrieval shard's chunk
	// concatenation against this value.
	MaxContextTokens int `json:"max_context_tokens"`

	// SHA256 is the hex-encoded SHA-256 of the GGUF artifact.
	// The runtime verifies the download against this value
	// before loading.
	SHA256 string `json:"sha256,omitempty"`

	// DownloadURL is the canonical URL for the GGUF artifact.
	// Empty when the catalog ships with embedded models.
	DownloadURL string `json:"download_url,omitempty"`
}

// ModelCatalog is the top-level shape returned by
// `GET /v1/models/catalog`. Versioned so a client can cache the
// catalog and re-fetch only when CatalogVersion bumps.
type ModelCatalog struct {
	// CatalogVersion is monotonic. Bumping it forces every device
	// to re-evaluate its model selection.
	CatalogVersion int64 `json:"catalog_version"`

	// Models is the list of distributable builds, ordered by
	// (family, quantization, tier_floor) for stable diffs.
	Models []ModelEntry `json:"models"`

	// EvictionConfig is the on-device shard eviction policy
	// table. Shipping it inside the catalog means the platform
	// can re-tune eviction without a client release. See
	// `internal/shard/eviction.go` for the policy contract.
	EvictionConfig []shard.EvictionPolicy `json:"eviction_config"`
}

// Validate returns an error when required fields are missing on
// any entry. Called once during NewStaticCatalog.
func (c *ModelCatalog) Validate() error {
	if c.CatalogVersion <= 0 {
		return errors.New("models: catalog_version required")
	}
	seen := make(map[string]struct{}, len(c.Models))
	for _, m := range c.Models {
		if m.ID == "" {
			return errors.New("models: entry id required")
		}
		if m.Family == "" {
			return errors.New("models: entry family required")
		}
		if m.TierFloor == shard.DeviceTierUnknown {
			return errors.New("models: entry tier_floor required")
		}
		if m.SizeMB <= 0 {
			return errors.New("models: entry size_mb must be positive")
		}
		if _, dup := seen[m.ID]; dup {
			return errors.New("models: duplicate entry id " + m.ID)
		}
		seen[m.ID] = struct{}{}
	}
	return nil
}

// Provider hands out the (read-only) catalog to the HTTP handler.
// Implementations may load the catalog from a signed file on disk
// (production) or a static literal (tests).
type Provider interface {
	Catalog() ModelCatalog
}

// StaticProvider is the default Provider — it returns a catalog
// baked into the binary at build time. Production may swap in a
// signed-file provider without changing the handler.
type StaticProvider struct {
	catalog ModelCatalog
}

// NewStaticCatalog returns the canonical Bonsai-1.7B catalog
// described in `docs/PROPOSAL.md` §9. Three quantizations align
// with the three device tiers:
//
//   - Low: no on-device SLM. The catalog ships no entry — the
//     server returns shard metadata + prefer_local=false so the
//     client falls back to remote retrieval.
//   - Mid: bonsai-1.7b-q4_0 (≈ 1.0 GB) at int4 quantization.
//   - High: bonsai-1.7b-q8_0 (≈ 1.8 GB) at int8 quantization plus
//     an fp16 build (≈ 3.4 GB) for users who explicitly opt in.
//
// The eviction config carries the three baseline policies from
// `shard.DefaultEvictionPolicies`.
func NewStaticCatalog() *StaticProvider {
	cat := ModelCatalog{
		CatalogVersion: 1,
		Models: []ModelEntry{
			{
				ID:               "bonsai-1.7b-q4_0",
				Name:             "Bonsai 1.7B (int4)",
				Family:           "bonsai-1.7b",
				TierFloor:        shard.DeviceTierMid,
				Quantization:     "q4_0",
				SizeMB:           1024,
				MaxContextTokens: 4096,
			},
			{
				ID:               "bonsai-1.7b-q8_0",
				Name:             "Bonsai 1.7B (int8)",
				Family:           "bonsai-1.7b",
				TierFloor:        shard.DeviceTierHigh,
				Quantization:     "q8_0",
				SizeMB:           1800,
				MaxContextTokens: 4096,
			},
			{
				ID:               "bonsai-1.7b-fp16",
				Name:             "Bonsai 1.7B (fp16)",
				Family:           "bonsai-1.7b",
				TierFloor:        shard.DeviceTierHigh,
				Quantization:     "fp16",
				SizeMB:           3400,
				MaxContextTokens: 4096,
			},
		},
		EvictionConfig: shard.DefaultEvictionPolicies(),
	}
	return &StaticProvider{catalog: cat}
}

// Catalog returns the catalog this provider was constructed with.
// Returned by value so callers can't mutate the internal state.
func (p *StaticProvider) Catalog() ModelCatalog {
	cp := p.catalog
	cp.Models = append([]ModelEntry(nil), p.catalog.Models...)
	cp.EvictionConfig = append([]shard.EvictionPolicy(nil), p.catalog.EvictionConfig...)
	return cp
}

// EligibleForTier returns the catalog entry whose TierFloor
// matches tier exactly. When multiple entries match (the High tier
// has q8_0 and fp16), the smallest one is returned so the runtime
// gets a sensible default. Returns nil when no entry matches.
func (c *ModelCatalog) EligibleForTier(tier shard.DeviceTier) *ModelEntry {
	if tier == shard.DeviceTierUnknown {
		return nil
	}
	var best *ModelEntry
	for i, m := range c.Models {
		if m.TierFloor != tier {
			continue
		}
		if best == nil || m.SizeMB < best.SizeMB {
			best = &c.Models[i]
		}
	}
	return best
}
