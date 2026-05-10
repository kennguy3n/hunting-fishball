package pipeline

// Round-6 Task 2 — semantic deduplication.
//
// Stage 4 optionally consults a Deduplicator before writing the
// (blocks, embeddings) pair to Qdrant + Postgres. The dedup pass
// detects pairs of chunks whose embeddings are above a configurable
// cosine-similarity threshold and drops the duplicate from the
// write set, recording an audit event so operators can trace why a
// chunk was suppressed.
//
// Enabling: CONTEXT_ENGINE_DEDUP_ENABLED=true
// Threshold (default 0.95): CONTEXT_ENGINE_DEDUP_THRESHOLD
//
// The implementation is deliberately self-contained — Stage 4 calls
// `Deduplicator.Apply` and the dedup decisions are emitted to the
// AuditSink. When the AuditSink is nil the worker still drops the
// duplicates but skips the audit emission.

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// DedupAuditSink is the narrow contract Stage 4 uses to record dedup
// decisions. *audit.Repository satisfies it transitively via the
// admin AuditWriter; tests inject an in-memory recorder.
type DedupAuditSink interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// DedupConfig configures the semantic deduplicator.
type DedupConfig struct {
	// Enabled disables the dedup pass when false. Defaults to
	// false for backward compatibility — operators must explicitly
	// opt in via env.
	Enabled bool

	// Threshold is the cosine-similarity floor above which two
	// chunks are considered duplicates. Defaults to 0.95.
	Threshold float32

	// Audit, when non-nil, receives one chunk.deduplicated entry
	// per suppressed chunk. Optional.
	Audit DedupAuditSink

	// Connector names the source connector for provenance in
	// the audit metadata. Set per-pipeline by the consumer wiring.
	Connector string
}

// LoadDedupConfigFromEnv parses CONTEXT_ENGINE_DEDUP_ENABLED /
// CONTEXT_ENGINE_DEDUP_THRESHOLD into a DedupConfig. Invalid values
// fall back to defaults — the deduplicator is fail-open.
func LoadDedupConfigFromEnv() DedupConfig {
	cfg := DedupConfig{Enabled: false, Threshold: 0.95}
	switch os.Getenv("CONTEXT_ENGINE_DEDUP_ENABLED") {
	case "1", "true", "TRUE", "True":
		cfg.Enabled = true
	}
	if raw := os.Getenv("CONTEXT_ENGINE_DEDUP_THRESHOLD"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 32); err == nil && v > 0 && v <= 1 {
			cfg.Threshold = float32(v)
		}
	}
	return cfg
}

// Deduplicator drops near-duplicate chunks from a Stage 4 write set.
type Deduplicator struct {
	cfg DedupConfig
}

// NewDeduplicator constructs a Deduplicator. cfg.Threshold defaults
// to 0.95 when zero.
func NewDeduplicator(cfg DedupConfig) *Deduplicator {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.95
	}
	return &Deduplicator{cfg: cfg}
}

// Enabled reports whether the dedup pass should run.
func (d *Deduplicator) Enabled() bool { return d != nil && d.cfg.Enabled }

// DedupResult summarises a single Apply call.
type DedupResult struct {
	Blocks     []Block
	Embeddings [][]float32
	Removed    int
}

// Apply walks (blocks, embeddings) and returns the deduplicated
// (blocks, embeddings) pair plus a count of suppressed chunks. The
// first occurrence in the slice wins; subsequent chunks whose cosine
// similarity to any already-kept chunk exceeds the threshold are
// dropped. When dedup is disabled Apply returns the inputs verbatim.
//
// Cosine similarity is computed on the (embedding) vectors directly
// — the inputs are assumed to be normalised by the embedding service
// (Phase 1 contract). If a vector is zero-length Apply skips
// the comparison and keeps the chunk.
func (d *Deduplicator) Apply(ctx context.Context, doc *Document, blocks []Block, embeddings [][]float32) (DedupResult, error) {
	if d == nil || !d.cfg.Enabled || len(blocks) < 2 {
		return DedupResult{Blocks: blocks, Embeddings: embeddings}, nil
	}
	if len(blocks) != len(embeddings) {
		return DedupResult{}, fmt.Errorf("dedup: block / embedding count mismatch %d vs %d", len(blocks), len(embeddings))
	}
	keptBlocks := make([]Block, 0, len(blocks))
	keptEmbeds := make([][]float32, 0, len(embeddings))
	keptIdx := make([]int, 0, len(blocks))
	removed := 0
	for i, b := range blocks {
		isDup := false
		var dupOf int
		var dupSim float32
		for k, j := range keptIdx {
			sim := cosineSimilarity(embeddings[i], embeddings[j])
			if sim >= d.cfg.Threshold {
				isDup = true
				dupOf = j
				dupSim = sim
				_ = k
				break
			}
		}
		if isDup {
			removed++
			d.recordDedup(ctx, doc, b, dupOf, dupSim)
			continue
		}
		keptBlocks = append(keptBlocks, b)
		keptEmbeds = append(keptEmbeds, embeddings[i])
		keptIdx = append(keptIdx, i)
	}
	return DedupResult{Blocks: keptBlocks, Embeddings: keptEmbeds, Removed: removed}, nil
}

func (d *Deduplicator) recordDedup(ctx context.Context, doc *Document, b Block, dupOfIdx int, sim float32) {
	if d.cfg.Audit == nil || doc == nil {
		return
	}
	meta := audit.JSONMap{
		"document_id":      doc.DocumentID,
		"source_id":        doc.SourceID,
		"block_id":         b.BlockID,
		"duplicate_of_idx": dupOfIdx,
		"similarity":       sim,
		"connector":        d.cfg.Connector,
		"threshold":        d.cfg.Threshold,
	}
	log := audit.NewAuditLog(doc.TenantID, "", audit.ActionChunkDeduplicated, "chunk", b.BlockID, meta, "")
	_ = d.cfg.Audit.Create(ctx, log)
}

// cosineSimilarity returns the cosine of the angle between a and b
// in [-1, 1]. Zero-length vectors return 0 (caller treats as "not
// similar").
func cosineSimilarity(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}
