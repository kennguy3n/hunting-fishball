# Bonsai-1.7B integration

## Why Bonsai-1.7B

The on-device SLM has to satisfy three constraints:

* fits in ≤2 GB resident memory on a Mid-tier device,
* maintains ≥12 tokens/sec decode on Mid-tier CPU,
* GGUF format so the same artefact serves
  llama.cpp / Bonsai-Rust / N-API bindings.

Bonsai-1.7B is the family that meets the envelope at three
quantizations: int4 (Mid floor), int8 (High floor), and fp16
(High preferred).

## Catalog

The server publishes a signed model catalog at
`GET /v1/models/catalog`. The Go-side type is
<ref:internal/models/catalog.go> `ModelCatalog`. JSON shape:

```json
{
  "catalog_version": 1,
  "models": [
    {
      "id": "bonsai-1.7b-q4_0",
      "name": "Bonsai 1.7B (int4)",
      "family": "bonsai-1.7b",
      "tier_floor": "mid",
      "quantization": "q4_0",
      "size_mb": 1024,
      "max_context_tokens": 4096,
      "sha256": "...",
      "download_url": "https://..."
    },
    { "id": "bonsai-1.7b-q8_0", "tier_floor": "high", ... },
    { "id": "bonsai-1.7b-fp16", "tier_floor": "high", ... }
  ],
  "eviction_config": [
    { "tier": "low",  "max_shard_size_mb": 64,   "min_free_memory_mb": 256,  "thermal_evict_multiplier": 0.5 },
    { "tier": "mid",  "max_shard_size_mb": 256,  "min_free_memory_mb": 512,  "thermal_evict_multiplier": 0.5 },
    { "tier": "high", "max_shard_size_mb": 1024, "min_free_memory_mb": 1024, "thermal_evict_multiplier": 0.5 }
  ]
}
```

## Tier eligibility

The client passes its self-reported `device_tier` to
`Provider.EligibleForTier(tier)`; the helper returns the
*smallest* model whose `tier_floor` ≤ tier. The tier order is
`low < mid < high`. Examples:

| device_tier | eligible (smallest first) | client picks |
|-------------|---------------------------|--------------|
| low | (none) | abort, server-only retrieval |
| mid | bonsai-1.7b-q4_0 | q4_0 |
| high | q4_0, q8_0, fp16 | q4_0 if storage-bound, else q8_0 / fp16 |

The client SHOULD prefer the smallest size that fits its budget;
a High-tier device with 4 GB free will pick fp16 for fidelity, a
High-tier device with 1.5 GB free falls back to q4_0.

## Eviction config

Per-tier eviction policy is part of the catalog so it can be
updated server-side without a client release. The Go-side type
is <ref:internal/shard/eviction.go> `EvictionPolicy`. The
on-device runtime calls `ShouldEvict()` whenever it considers
caching a new shard; the decision tree is:

1. `device_tier == unknown` → evict (the runtime has no policy
   floor for an unclassified device).
2. shard size > `max_shard_size_mb` (× `thermal_evict_multiplier`
   when thermal-throttled) → evict.
3. available memory < `min_free_memory_mb` → evict.
4. otherwise keep.

The `thermal_evict_multiplier` halves the max shard size when
the OS reports thermal throttling (iOS `ProcessInfo.thermalState`
≥ `.serious`, Android `PowerManager.THERMAL_STATUS_SEVERE`,
desktop `powerMonitor.getCurrentThermalState() === "critical"`).

## Catalog signing (future)

Phase 8 ships an unsigned catalog. The post-Phase-8 milestone
adds an Ed25519 signature over the catalog body that clients
verify before using it. The Go-side `ModelCatalog` already has
space for the signature (a future `signature` field on
`Provider.Catalog()`); the catalog handler will gain a
`KEY_FILE` env var to point at the signing key.

## Download flow

1. Client calls `GET /v1/models/catalog` on warm-boot.
2. Client compares each model's `sha256` against locally cached
   blobs. If a match exists, no download.
3. Otherwise client streams the GGUF file from `download_url`,
   verifies the SHA-256 against the catalog entry, and atomically
   moves the file into the runtime's model directory.
4. The runtime memory-maps the file on first use.

## Server-side validation

The Go-side `ModelCatalog.Validate()` rejects:

* `catalog_version <= 0`
* a model with `id == ""`, `family == ""`, or `tier_floor == ""`
* a model with `size_mb <= 0`
* duplicate model ids

`StaticProvider` runs `Validate()` at construction so a
hard-coded catalog with a regression cannot ship.
