package retrieval

// chunk_merger.go — Round-19 Task 23.
//
// Post-rerank chunk merger. Combines adjacent / co-document
// matches whose individual Text length falls under a configurable
// threshold into a single Match with concatenated Text. The
// retrieval caller (LLM, summariser) sees fewer, larger context
// windows — a measurable quality improvement when the underlying
// chunking strategy carved a paragraph into 3-4 tiny blocks.
//
// Gated behind CONTEXT_ENGINE_CHUNK_MERGE_ENABLED. When disabled
// the function is a no-op, returning the input matches unchanged.
//
// Design constraints:
//
//   - Order-preserving: the merger must preserve the input
//     match order so the top-1 result of the reranker remains
//     the top-1 result of the merger.
//   - Per-document: only adjacent matches sharing the same
//     (TenantID, DocumentID) are merged. We never glue matches
//     from different documents together.
//   - Score-preserving: the merged match takes the maximum
//     Score (and the maximum OriginalScore) of the contributing
//     matches so reranker semantics survive a merge.
//   - Bounded: stops merging once the cumulative Text exceeds
//     MaxMergedTextChars. Avoids constructing a single
//     pathological match the caller can't actually use.

import (
	"strings"
)

// ChunkMergerConfig configures the post-rerank chunk merger.
type ChunkMergerConfig struct {
	// Enabled gates the merger. When false ChunkMerger.Merge is a
	// no-op. Mirror of CONTEXT_ENGINE_CHUNK_MERGE_ENABLED.
	Enabled bool

	// MinTextChars is the per-match short-text threshold below
	// which a match is a candidate for merging. Defaults to 256
	// when zero.
	MinTextChars int

	// MaxMergedTextChars is the upper bound on the merged Text
	// length. Defaults to 4096 when zero.
	MaxMergedTextChars int

	// MaxGroupSize is the upper bound on the number of source
	// matches glued into one merged match. Defaults to 8 when
	// zero.
	MaxGroupSize int

	// Separator is inserted between merged text bodies. Defaults
	// to "\n\n" when empty.
	Separator string
}

// ChunkMerger is the post-rerank merger.
type ChunkMerger struct {
	cfg ChunkMergerConfig
}

// NewChunkMerger validates cfg and returns a *ChunkMerger.
// Round-19 Task 23.
func NewChunkMerger(cfg ChunkMergerConfig) *ChunkMerger {
	if cfg.MinTextChars == 0 {
		cfg.MinTextChars = 256
	}
	if cfg.MaxMergedTextChars == 0 {
		cfg.MaxMergedTextChars = 4096
	}
	if cfg.MaxGroupSize == 0 {
		cfg.MaxGroupSize = 8
	}
	if cfg.Separator == "" {
		cfg.Separator = "\n\n"
	}

	return &ChunkMerger{cfg: cfg}
}

// Enabled reports whether the merger is active.
func (m *ChunkMerger) Enabled() bool { return m != nil && m.cfg.Enabled }

// Merge collapses adjacent same-document matches whose Text is
// shorter than MinTextChars into a single merged Match. Returns
// the input unchanged when Enabled is false.
//
// The function is total: every input match is reachable in the
// returned slice (either as-is or as part of a merged group).
func (m *ChunkMerger) Merge(in []Match) []Match {
	if m == nil || !m.cfg.Enabled || len(in) == 0 {
		return in
	}
	out := make([]Match, 0, len(in))
	i := 0
	for i < len(in) {
		base := in[i]
		// Only short, document-anchored matches are candidates.
		if base.DocumentID == "" || base.TenantID == "" || len(base.Text) >= m.cfg.MinTextChars {
			out = append(out, base)
			i++

			continue
		}
		group := []Match{base}
		j := i + 1
		totalLen := len(base.Text)
		for j < len(in) && len(group) < m.cfg.MaxGroupSize {
			cand := in[j]
			if cand.TenantID != base.TenantID || cand.DocumentID != base.DocumentID {
				break
			}
			if len(cand.Text) >= m.cfg.MinTextChars {
				break
			}
			nextLen := totalLen + len(m.cfg.Separator) + len(cand.Text)
			if nextLen > m.cfg.MaxMergedTextChars {
				break
			}
			group = append(group, cand)
			totalLen = nextLen
			j++
		}
		if len(group) == 1 {
			out = append(out, base)
			i++

			continue
		}
		out = append(out, mergeGroup(group, m.cfg.Separator))
		i = j
	}

	return out
}

// mergeGroup collapses a same-document run of matches into a
// single representative Match. The first element provides the
// identity fields (ID, BlockID, URI) so downstream consumers
// keep a stable reference; the merged Text concatenates each
// member's body with `sep` between bodies.
func mergeGroup(group []Match, sep string) Match {
	head := group[0]
	bodies := make([]string, 0, len(group))
	sources := make(map[string]struct{}, len(group)*2)
	for _, src := range head.Sources {
		sources[src] = struct{}{}
	}
	maxScore := head.Score
	maxOrig := head.OriginalScore
	for _, g := range group {
		if g.Text != "" {
			bodies = append(bodies, g.Text)
		}
		for _, src := range g.Sources {
			sources[src] = struct{}{}
		}
		if g.Score > maxScore {
			maxScore = g.Score
		}
		if g.OriginalScore > maxOrig {
			maxOrig = g.OriginalScore
		}
	}
	merged := head
	merged.Text = strings.Join(bodies, sep)
	merged.Score = maxScore
	merged.OriginalScore = maxOrig
	merged.Sources = make([]string, 0, len(sources))
	for src := range sources {
		merged.Sources = append(merged.Sources, src)
	}
	if merged.Metadata == nil {
		merged.Metadata = map[string]any{}
	} else {
		// Shallow copy so we don't mutate the caller's match.
		clone := make(map[string]any, len(merged.Metadata)+2)
		for k, v := range merged.Metadata {
			clone[k] = v
		}
		merged.Metadata = clone
	}
	merged.Metadata["chunk_merger_group_size"] = len(group)
	memberIDs := make([]string, 0, len(group))
	for _, g := range group {
		if g.ID != "" {
			memberIDs = append(memberIDs, g.ID)
		}
	}
	merged.Metadata["chunk_merger_member_ids"] = memberIDs

	return merged
}
