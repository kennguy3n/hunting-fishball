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
	return exp
}
