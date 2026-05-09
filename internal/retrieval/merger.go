package retrieval

// Phase 3 result merger.
//
// The retrieval API runs four backends (vector, BM25, graph, memory)
// in parallel (ARCHITECTURE.md §4.2) and fuses their ranked results
// via Reciprocal Rank Fusion. RRF formula:
//
//	score(d) = sum over each stream s of  1 / (k + rank_s(d))
//
// where `k` is a constant (RRF paper recommends 60) and `rank_s(d)`
// is the 1-based position of `d` in stream `s` (omitted when the
// stream did not return `d`).
//
// The merger preserves the per-source provenance — it never drops a
// hit just because another stream out-scored it; it only re-scores.
// The reranker (reranker.go) is what decides whether to keep a hit
// after merging, and the policy filter is what decides whether the
// caller is even allowed to see it.

import (
	"sort"
	"time"
)

// Source identifies which backend a Match came from. Stable strings so
// the response policy field can name them.
const (
	SourceVector = "vector"
	SourceBM25   = "bm25"
	SourceGraph  = "graph"
	SourceMemory = "memory"
)

// Match is the merger's input/output unit. The four backend clients
// each project their native hit into a Match before submitting it to
// the merger.
type Match struct {
	// ID is the canonical chunk identifier (vector/BM25 share it
	// directly; graph/memory project their entity / record id into
	// the same field). Required.
	ID string

	// Source names the backend the match came from
	// (SourceVector / SourceBM25 / SourceGraph / SourceMemory).
	Source string

	// Score is the source-specific score (cosine similarity, BM25
	// score, etc). Used by the reranker; the merger uses rank, not
	// score.
	Score float32

	// Rank is the 1-based rank of the match within its source list.
	// The merger uses Rank when supplied; if Rank is 0, the merger
	// derives it from the slice index.
	Rank int

	// PrivacyLabel is the chunk's privacy label propagated through
	// the pipeline. The policy filter uses it.
	PrivacyLabel string

	// IngestedAt is the wall clock when the chunk was first written.
	// The reranker uses it for the freshness boost.
	IngestedAt time.Time

	// TenantID / DocumentID / BlockID / Title / URI / Text /
	// Connector / Metadata mirror retrieval.RetrieveHit so the
	// handler can project a Match back into the response without
	// re-fetching from Postgres.
	TenantID   string
	SourceID   string
	DocumentID string
	BlockID    string
	Title      string
	URI        string
	Text       string
	Connector  string
	Metadata   map[string]any

	// Sources records every backend that contributed to the merged
	// match. Populated by the merger.
	Sources []string
}

// MergerConfig configures the RRF merger.
type MergerConfig struct {
	// K is the RRF constant. Defaults to 60 per the original RRF
	// paper.
	K int

	// PerSourceWeight optionally scales each backend's contribution.
	// Default weight is 1.0 for every stream. Use 0 to disable a
	// stream's contribution while still passing it through (e.g.
	// "memory contributed but is non-evidential — score it at zero
	// but still surface it in policy.applied").
	PerSourceWeight map[string]float32
}

// Merger fuses ranked result streams via Reciprocal Rank Fusion.
type Merger struct {
	cfg MergerConfig
}

// NewMerger returns a Merger.
func NewMerger(cfg MergerConfig) *Merger {
	if cfg.K <= 0 {
		cfg.K = 60
	}

	return &Merger{cfg: cfg}
}

// Merge fuses one or more streams of *Match into a single descending
// score-ordered list. The same chunk_id appearing in multiple streams
// is collapsed into one Match whose `Sources` lists every contributing
// backend and whose Score is the RRF sum.
func (m *Merger) Merge(streams ...[]*Match) []*Match {
	scored := map[string]*Match{}

	for _, stream := range streams {
		for i, hit := range stream {
			if hit == nil || hit.ID == "" {
				continue
			}
			rank := hit.Rank
			if rank <= 0 {
				rank = i + 1
			}
			weight := float32(1)
			if m.cfg.PerSourceWeight != nil {
				if w, ok := m.cfg.PerSourceWeight[hit.Source]; ok {
					weight = w
				}
			}
			contribution := weight / float32(m.cfg.K+rank)

			existing, ok := scored[hit.ID]
			if !ok {
				existing = cloneMatch(hit)
				existing.Score = 0
				existing.Sources = nil
				scored[hit.ID] = existing
			}
			existing.Score += contribution
			existing.Sources = appendUnique(existing.Sources, hit.Source)
			// Keep the richest provenance: the first non-empty value
			// from any stream wins.
			fillMissing(existing, hit)
		}
	}

	out := make([]*Match, 0, len(scored))
	for _, m := range scored {
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ID < out[j].ID
		}

		return out[i].Score > out[j].Score
	})

	return out
}

// cloneMatch returns a shallow copy of m suitable for the merger's
// per-id accumulator.
func cloneMatch(m *Match) *Match {
	cp := *m
	if len(m.Sources) > 0 {
		cp.Sources = append([]string(nil), m.Sources...)
	}
	if len(m.Metadata) > 0 {
		cp.Metadata = make(map[string]any, len(m.Metadata))
		for k, v := range m.Metadata {
			cp.Metadata[k] = v
		}
	}

	return &cp
}

// fillMissing copies provenance fields from src into dst when dst
// hasn't seen them yet. Lets the merger surface as much provenance as
// any contributing stream emitted.
func fillMissing(dst, src *Match) {
	if dst.TenantID == "" {
		dst.TenantID = src.TenantID
	}
	if dst.SourceID == "" {
		dst.SourceID = src.SourceID
	}
	if dst.DocumentID == "" {
		dst.DocumentID = src.DocumentID
	}
	if dst.BlockID == "" {
		dst.BlockID = src.BlockID
	}
	if dst.Title == "" {
		dst.Title = src.Title
	}
	if dst.URI == "" {
		dst.URI = src.URI
	}
	if dst.Text == "" {
		dst.Text = src.Text
	}
	if dst.Connector == "" {
		dst.Connector = src.Connector
	}
	if dst.PrivacyLabel == "" {
		dst.PrivacyLabel = src.PrivacyLabel
	}
	if dst.IngestedAt.IsZero() {
		dst.IngestedAt = src.IngestedAt
	}
	if dst.Metadata == nil && len(src.Metadata) > 0 {
		dst.Metadata = make(map[string]any, len(src.Metadata))
	}
	for k, v := range src.Metadata {
		if _, ok := dst.Metadata[k]; !ok {
			dst.Metadata[k] = v
		}
	}
}

// appendUnique returns slice with v appended iff it isn't already
// present.
func appendUnique(slice []string, v string) []string {
	if v == "" {
		return slice
	}
	for _, x := range slice {
		if x == v {
			return slice
		}
	}

	return append(slice, v)
}
