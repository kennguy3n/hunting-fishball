package retrieval

// Phase 4 / Round-6 Task 1 — Maximal Marginal Relevance (MMR)
// diversifier.
//
// MMR re-ranks the post-rerank candidate list to balance relevance
// against diversity. The standard MMR scoring rule, with lambda in
// [0, 1], is:
//
//	score(d) = lambda * Rel(d, q) - (1 - lambda) * max_{d' in S} Sim(d, d')
//
// where S is the set of already-selected results, Rel(d, q) is the
// candidate's relevance to the query (we use the post-rerank
// `Match.Score` as the proxy), and Sim(d, d') is the pairwise
// similarity between two candidates.
//
// A `lambda` of 0 means "pure relevance / no diversification" so the
// caller's existing rank is preserved. `lambda` of 1 means "pure
// diversity" — pick the most relevant first, then greedily prefer
// candidates that look least like everything already chosen, ignoring
// the per-candidate score difference. Mid-range values blend the two.
//
// The diversifier is wired into the retrieval handler immediately
// after `Reranker.Rerank()` and before the policy filter. Callers
// opt-in via `RetrieveRequest.Diversity`; the default of 0.0 keeps
// the legacy passthrough behaviour intact.

import (
	"context"
	"math"
	"strings"
	"unicode"
)

// SimilarityFunc returns a [0, 1] similarity score between two
// matches. The default token-Jaccard implementation
// (jaccardSimilarity) is text-based so MMR works even when the
// fan-out backends don't surface embeddings on `Match`.
type SimilarityFunc func(a, b *Match) float32

// Diversifier is the seam between the retrieval handler and the
// MMR algorithm. The default implementation is *MMRDiversifier.
type Diversifier interface {
	// Diversify returns a re-ordered slice of matches with at most
	// `topK` items. Implementations must be a no-op when
	// `lambda == 0` so callers can dial diversification on/off
	// without rewriting their request bodies.
	Diversify(ctx context.Context, matches []*Match, lambda float32, topK int) []*Match
}

// MMRDiversifier is the default MMR implementation.
type MMRDiversifier struct {
	// Similarity is the pairwise similarity function. Defaults to
	// `jaccardSimilarity` over the matches' Text + Title.
	Similarity SimilarityFunc
}

// NewMMRDiversifier returns an MMR diversifier with sensible defaults.
func NewMMRDiversifier(sim SimilarityFunc) *MMRDiversifier {
	if sim == nil {
		sim = jaccardSimilarity
	}
	return &MMRDiversifier{Similarity: sim}
}

// Diversify implements the MMR re-ranking. Returns the original slice
// unchanged when `lambda <= 0` (passthrough) or when there is nothing
// to diversify (len < 2). When `topK <= 0` the diversifier returns
// the full re-ordered slice.
func (d *MMRDiversifier) Diversify(ctx context.Context, matches []*Match, lambda float32, topK int) []*Match {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return matches
		}
	}
	if lambda <= 0 || len(matches) < 2 {
		if topK > 0 && topK < len(matches) {
			return matches[:topK]
		}
		return matches
	}
	if lambda > 1 {
		lambda = 1
	}
	limit := len(matches)
	if topK > 0 && topK < limit {
		limit = topK
	}

	// Normalise relevance scores to [0, 1] so lambda has a stable
	// interpretation regardless of the underlying scorer's range.
	rel := normalisedScores(matches)

	selected := make([]*Match, 0, limit)
	selectedIdx := make([]int, 0, limit)
	picked := make([]bool, len(matches))

	for len(selected) < limit {
		bestIdx := -1
		bestScore := float32(math.Inf(-1))
		for i, m := range matches {
			if picked[i] || m == nil {
				continue
			}
			var maxSim float32
			for _, j := range selectedIdx {
				s := d.Similarity(m, matches[j])
				if s > maxSim {
					maxSim = s
				}
			}
			score := lambda*rel[i] - (1-lambda)*maxSim
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}
		picked[bestIdx] = true
		selectedIdx = append(selectedIdx, bestIdx)
		selected = append(selected, matches[bestIdx])
	}
	return selected
}

// normalisedScores returns matches' Score values rescaled into
// [0, 1] using min-max normalisation. When all scores are equal the
// result is uniform 1.0 (so MMR falls back to pure-diversity
// ordering when the reranker can't separate the candidates).
func normalisedScores(matches []*Match) []float32 {
	out := make([]float32, len(matches))
	if len(matches) == 0 {
		return out
	}
	minScore := float32(math.Inf(1))
	maxScore := float32(math.Inf(-1))
	for _, m := range matches {
		if m == nil {
			continue
		}
		if m.Score < minScore {
			minScore = m.Score
		}
		if m.Score > maxScore {
			maxScore = m.Score
		}
	}
	if maxScore == minScore {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	span := maxScore - minScore
	for i, m := range matches {
		if m == nil {
			continue
		}
		out[i] = (m.Score - minScore) / span
	}
	return out
}

// jaccardSimilarity computes a token-level Jaccard similarity over
// the matches' Text (falling back to Title when Text is empty). The
// tokeniser splits on Unicode word boundaries and lower-cases the
// result. Identical or empty inputs produce 1.0; disjoint inputs
// produce 0.0.
func jaccardSimilarity(a, b *Match) float32 {
	if a == nil || b == nil {
		return 0
	}
	if a == b || a.ID == b.ID {
		return 1
	}
	tokensA := tokenSet(matchText(a))
	tokensB := tokenSet(matchText(b))
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}
	intersect := 0
	for tok := range tokensA {
		if _, ok := tokensB[tok]; ok {
			intersect++
		}
	}
	union := len(tokensA) + len(tokensB) - intersect
	if union == 0 {
		return 0
	}
	return float32(intersect) / float32(union)
}

func matchText(m *Match) string {
	if m == nil {
		return ""
	}
	if strings.TrimSpace(m.Text) != "" {
		return m.Text
	}
	return m.Title
}

// tokenSet lowercases s and returns the set of word tokens.
func tokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	if s == "" {
		return out
	}
	field := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), field) {
		if tok == "" {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}
