// explain.go — Round-5 Task 12.
//
// When a retrieval request sets `"explain": true` AND the caller is
// either an admin (RBAC) or the deployment opted in via the
// CONTEXT_ENGINE_EXPLAIN_ENABLED env var, the response includes a
// per-hit `explain` block that exposes the raw signal scores and
// the policy verdict the merger + reranker + policy filter
// computed. This is a debug surface used by support engineers and
// a feature-flagged research surface for evaluating reranker
// changes.
//
// Explain is intentionally not part of the public retrieval
// contract — it leaks scoring internals and is not stable across
// releases.
package retrieval

import (
	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// ExplainEnvVar is the deployment-wide opt-in to enable explain
// mode for non-admin callers. Mirrors the pattern used by the
// retrieval cache + backend gates.
const ExplainEnvVar = "CONTEXT_ENGINE_EXPLAIN_ENABLED"

// RetrievalExplain is the per-hit debug projection. Every field is
// optional — a backend that didn't contribute leaves its score at
// zero. Sources lists every backend that returned this match,
// matching Match.Sources.
type RetrievalExplain struct {
	// VectorSimilarity is the cosine similarity returned by the
	// vector backend (Qdrant). Zero when vector did not contribute.
	VectorSimilarity float32 `json:"vector_similarity,omitempty"`

	// BM25Score is the lexical relevance score returned by the
	// BM25 backend (Tantivy). Zero when BM25 did not contribute.
	BM25Score float32 `json:"bm25_score,omitempty"`

	// GraphTraversalDepth is the entity-graph hops the graph
	// backend reported. Zero means the graph backend did not
	// contribute or did not return depth metadata.
	GraphTraversalDepth int `json:"graph_traversal_depth,omitempty"`

	// MemoryScore is the persistent-memory backend's match score.
	// Zero when memory did not contribute.
	MemoryScore float32 `json:"memory_score,omitempty"`

	// RRFRank is the merger's 1-based fused rank for the hit.
	// Zero means the merger did not run (single-stream response).
	RRFRank int `json:"rrf_rank,omitempty"`

	// RRFScore is the post-merger fused score (sum of 1/(k+rank)
	// across contributing backends).
	RRFScore float32 `json:"rrf_score,omitempty"`

	// RerankerAdjustment is the delta the reranker applied on top
	// of the merger's RRF score. A positive value means the
	// reranker boosted the hit; a negative value means the
	// reranker pushed it down.
	RerankerAdjustment float32 `json:"reranker_adjustment,omitempty"`

	// PolicyDecision is "allowed" for hits that survived the
	// policy filter, or the human-readable reason a hit was
	// blocked when surfaced via the blocked-but-explain admin
	// channel. The default response only carries allowed hits, so
	// this is "allowed" in nearly every case.
	PolicyDecision string `json:"policy_decision,omitempty"`

	// Sources echoes Match.Sources so the client can see exactly
	// which backends contributed to the hit without re-deriving
	// it from the score fields above.
	Sources []string `json:"sources,omitempty"`

	// FreshnessBoost — Round-18 Task 14. Captures the linear
	// reranker's freshness lift component so an operator can see
	// why a fresh chunk topped an older but more relevant one.
	// Zero when the reranker did not run or freshness weighting
	// is disabled.
	FreshnessBoost float32 `json:"freshness_boost,omitempty"`

	// PinBoost — Round-18 Task 14. Non-zero when the chunk is
	// pinned (see internal/admin/pinned_results.go). The boost is
	// the amount the pin filter added to the reranker's score so
	// operators can see the pin's effect at a glance.
	PinBoost float32 `json:"pin_boost,omitempty"`

	// MMRDiversityPenalty — Round-18 Task 14. Negative value
	// applied by the MMR (maximum marginal relevance) post-rerank
	// step when the chunk is too similar to a higher-ranked hit.
	// Zero when MMR did not run or did not penalise the chunk.
	MMRDiversityPenalty float32 `json:"mmr_diversity_penalty,omitempty"`

	// CrossEncoderScore — Round-18 Task 14. The relevance score
	// the cross-encoder reranker returned. Zero when the
	// cross-encoder reranker did not run (LinearReranker path).
	CrossEncoderScore float32 `json:"cross_encoder_score,omitempty"`

	// BM25TermContributions — Round-18 Task 14. Optional
	// breakdown of which query terms contributed how much to the
	// BM25 score. Populated only when the BM25 adapter reports
	// per-term tf-idf; otherwise omitted.
	BM25TermContributions map[string]float32 `json:"bm25_term_contributions,omitempty"`
}

// IsExplainAuthorized returns true when the caller's RBAC role
// (read from the gin.Context via admin.RoleFromContext) satisfies
// the admin requirement OR the deployment opted in via the
// ExplainEnvVar feature flag passed in envEnabled.
//
// envEnabled is the resolved value of the CONTEXT_ENGINE_EXPLAIN_ENABLED
// flag — the handler computes it once at startup and passes it
// here per-request so the auth check stays decoupled from os.Getenv.
func IsExplainAuthorized(c *gin.Context, envEnabled bool) bool {
	if envEnabled {
		return true
	}
	role, ok := admin.RoleFromContext(c)
	if !ok {
		return false
	}
	return role == admin.RoleAdmin || role == admin.RoleSuperAdmin
}

// BuildExplain projects an internal *Match into the public
// RetrievalExplain shape. Match.Score is the post-merger RRF score
// and Match.Rank is the merger's fused rank.
//
// Match.OriginalScore is intentionally backend-specific: per
// merger.go:63 it is "currently populated for SourceBM25 only" —
// the merger preserves it when the BM25 stream contributes a hit
// so the reranker can blend the lexical signal in. So we only
// project OriginalScore onto the BM25 explain field; for other
// contributing backends we leave their score zero (omitted from
// the JSON via omitempty) until they grow per-source score
// preservation. Assigning OriginalScore to VectorSimilarity /
// MemoryScore on a multi-source match would falsely report the
// BM25 score as the vector cosine or memory match, which the
// merger never computed.
//
// Pre-rerank and post-rerank scores are tracked separately on the
// match so the reranker adjustment can be surfaced as a delta.
// When the reranker did not run, RerankerAdjustment is zero.
func BuildExplain(m *Match, preRerankScore float32) *RetrievalExplain {
	if m == nil {
		return nil
	}
	exp := &RetrievalExplain{
		RRFScore:           preRerankScore,
		RRFRank:            m.Rank,
		RerankerAdjustment: m.Score - preRerankScore,
		PolicyDecision:     "allowed",
		Sources:            append([]string(nil), m.Sources...),
	}
	for _, s := range m.Sources {
		switch s {
		case SourceBM25:
			// OriginalScore is BM25-specific (merger.go:63) so we
			// only attribute it here.
			exp.BM25Score = m.OriginalScore
		case SourceGraph:
			// Graph traversal depth lives in metadata under the
			// "graph_traversal_depth" key when the graph adapter
			// populates it. Fall back to 1 (direct hop) when the
			// adapter only returns a flat list.
			if d, ok := m.Metadata["graph_traversal_depth"].(int); ok {
				exp.GraphTraversalDepth = d
			} else if exp.GraphTraversalDepth == 0 {
				exp.GraphTraversalDepth = 1
			}
		case SourceVector, SourceMemory:
			// OriginalScore is not preserved for these backends
			// today (only BM25 sets it). Leaving the field zero
			// is more honest than falsely projecting a BM25
			// number. When per-source score tracking lands, set
			// the appropriate field here.
		}
	}
	// Round-18 Task 14 — surface per-chunk reranker breakdown
	// when adapters dropped it into Metadata. Adapters that
	// don't populate the keys leave the JSON fields omitted.
	if m.Metadata != nil {
		if v, ok := metadataFloat32(m.Metadata, "freshness_boost"); ok {
			exp.FreshnessBoost = v
		}
		if v, ok := metadataFloat32(m.Metadata, "pin_boost"); ok {
			exp.PinBoost = v
		}
		if v, ok := metadataFloat32(m.Metadata, "mmr_diversity_penalty"); ok {
			exp.MMRDiversityPenalty = v
		}
		if v, ok := metadataFloat32(m.Metadata, "cross_encoder_score"); ok {
			exp.CrossEncoderScore = v
		}
		if terms, ok := m.Metadata["bm25_term_contributions"].(map[string]float32); ok && len(terms) > 0 {
			exp.BM25TermContributions = terms
		}
	}
	return exp
}

// metadataFloat32 reads a numeric metadata field as a float32,
// tolerating the float32 / float64 / int duality JSON decoding
// introduces.
func metadataFloat32(meta map[string]any, key string) (float32, bool) {
	v, ok := meta[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float32:
		return x, true
	case float64:
		return float32(x), true
	case int:
		return float32(x), true
	}

	return 0, false
}

// computeBackendContributions — Round-13 Task 7.
//
// Counts how many top-K hits each backend contributed by walking
// Match.Sources for every presented match. A single match that
// surfaced on multiple backends increments the count for each.
// The returned map is keyed by the same SourceVector / SourceBM25
// / SourceGraph / SourceMemory constants the backends use.
// Returns nil when matches is empty so the JSON omitempty drops
// the field rather than emitting {}.
func computeBackendContributions(matches []*Match) map[string]int {
	if len(matches) == 0 {
		return nil
	}
	out := make(map[string]int, 4)
	for _, m := range matches {
		if m == nil {
			continue
		}
		for _, s := range m.Sources {
			out[s]++
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
