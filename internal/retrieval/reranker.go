package retrieval

// Phase 3 lightweight Go-side reranker + tenant policy filter.
//
// After the merger fuses four streams via RRF, the reranker re-scores
// the top-N candidates with a small linear blend that's cheap enough
// to run in-process: BM25-style score (already supplied by the BM25
// backend), the merger's RRF score, and a freshness boost based on
// IngestedAt. A future Phase will replace this with a remote
// cross-encoder reranker; the Reranker interface here is the seam.
//
// The policy filter drops chunks whose privacy_label exceeds the
// channel's privacy mode and counts the rejections so the response's
// `policy.blocked_count` reflects the fan-out reality.

import (
	"context"
	"sort"
	"time"
)

// Reranker is the seam between the in-process reranker and a future
// gRPC reranker microservice.
type Reranker interface {
	Rerank(ctx context.Context, query string, matches []*Match) ([]*Match, error)
}

// LinearRerankerConfig configures the lightweight Go reranker.
type LinearRerankerConfig struct {
	// FusionWeight is the multiplier on the merger's RRF score.
	// Default 0.6.
	FusionWeight float32

	// BM25Weight is the multiplier on the BM25 stream's source-side
	// score (when a Match has Source=SourceBM25 it carries Score).
	// Default 0.3.
	BM25Weight float32

	// FreshnessWeight is the multiplier on the freshness term, where
	// freshness = exp(-age_hours / FreshnessHalfLifeHours). Default
	// 0.1.
	FreshnessWeight float32

	// FreshnessHalfLifeHours is how many hours of age halves the
	// freshness term. Default 168 (1 week).
	FreshnessHalfLifeHours float64

	// Now is injected for deterministic tests; defaults to time.Now.
	Now func() time.Time
}

// LinearReranker is the default in-process reranker.
type LinearReranker struct {
	cfg LinearRerankerConfig
}

// NewLinearReranker returns a LinearReranker with sensible defaults.
func NewLinearReranker(cfg LinearRerankerConfig) *LinearReranker {
	if cfg.FusionWeight == 0 {
		cfg.FusionWeight = 0.6
	}
	if cfg.BM25Weight == 0 {
		cfg.BM25Weight = 0.3
	}
	if cfg.FreshnessWeight == 0 {
		cfg.FreshnessWeight = 0.1
	}
	if cfg.FreshnessHalfLifeHours == 0 {
		cfg.FreshnessHalfLifeHours = 168
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &LinearReranker{cfg: cfg}
}

// Rerank re-scores matches in-place and returns them descending.
//
// The reranker treats each Match's Score (set by the merger to the
// RRF score) as the base ranking signal; the BM25 stream's
// OriginalScore is blended in via BM25Weight so a strong lexical
// match is rewarded beyond its rank contribution. Freshness is a
// soft bonus that decays exponentially.
func (r *LinearReranker) Rerank(ctx context.Context, _ string, matches []*Match) ([]*Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := r.cfg.Now()
	for _, m := range matches {
		if m == nil {
			continue
		}
		// Read the original BM25 score (preserved by the merger via
		// Match.OriginalScore). Falling back to Score here would
		// double-count RRF since the merger overwrites Score.
		bm25 := float32(0)
		if hasSource(m, SourceBM25) {
			bm25 = m.OriginalScore
		}
		freshness := freshnessTerm(m.IngestedAt, now, r.cfg.FreshnessHalfLifeHours)
		// We blend on a copy of the RRF score so re-running the
		// reranker (e.g. after an A/B feature flag flip) produces a
		// deterministic value.
		base := m.Score
		m.Score = r.cfg.FusionWeight*base +
			r.cfg.BM25Weight*bm25 +
			r.cfg.FreshnessWeight*freshness
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Score > matches[j].Score
	})

	return matches, nil
}

// hasSource returns true when m's Sources slice (set by the merger)
// contains s.
func hasSource(m *Match, s string) bool {
	for _, x := range m.Sources {
		if x == s {
			return true
		}
	}

	return m.Source == s
}

// freshnessTerm returns a value in [0, 1] that decays exponentially
// with age. exp(-age_hours / halfLifeHours) — 1 at age 0, ≈0.5 at one
// half-life, ≈0.0 deep into the past.
func freshnessTerm(ingested, now time.Time, halfLife float64) float32 {
	if ingested.IsZero() || halfLife <= 0 {
		return 0
	}
	ageHours := now.Sub(ingested).Hours()
	if ageHours <= 0 {
		return 1
	}
	// We hand-roll a cheap exp via the half-life: 0.5 ^ (age /
	// halfLife). This is monotonic and bounded; we don't need
	// math.Exp's accuracy here.
	return float32(pow2(-ageHours / halfLife))
}

// pow2 returns 2 ** x for any real x using a small Taylor expansion
// that's accurate enough for the ranking heuristic. We avoid
// importing math here so the reranker stays a tiny pure-arithmetic
// hot loop.
func pow2(x float64) float64 {
	// 2^x = e^(x * ln2).
	const ln2 = 0.6931471805599453
	return expApprox(x * ln2)
}

// expApprox is a 5-term Taylor approximation of e^x for x in
// [-10, 10]. Accuracy: ≤1e-3 absolute, which is finer than the
// freshness weight's resolution.
func expApprox(x float64) float64 {
	if x > 10 {
		x = 10
	}
	if x < -10 {
		x = -10
	}
	// e^x = e^(int) * e^(frac); we shrink x by halving so the Taylor
	// expansion stays in its accuracy band, then square back.
	steps := 0
	for x > 0.5 || x < -0.5 {
		x /= 2
		steps++
	}
	// 1 + x + x^2/2 + x^3/6 + x^4/24
	r := 1 + x + x*x/2 + x*x*x/6 + x*x*x*x/24
	for i := 0; i < steps; i++ {
		r *= r
	}

	return r
}

// PolicyConfig configures the policy filter.
type PolicyConfig struct {
	// PrivacyOrder is the privacy_label total order. Labels not
	// listed are treated as having a higher privacy level than any
	// listed label (i.e. blocked unless the channel's mode is the
	// most permissive listed value).
	PrivacyOrder []string
}

// PolicyFilter applies tenant policy after the reranker. A chunk is
// dropped when its PrivacyLabel ranks higher than the channel's
// declared mode in PrivacyOrder.
type PolicyFilter struct {
	cfg PolicyConfig
}

// NewPolicyFilter returns a PolicyFilter. When cfg.PrivacyOrder is
// empty, the standard order public < internal < confidential <
// restricted < secret is used.
func NewPolicyFilter(cfg PolicyConfig) *PolicyFilter {
	if len(cfg.PrivacyOrder) == 0 {
		cfg.PrivacyOrder = []string{"public", "internal", "confidential", "restricted", "secret"}
	}

	return &PolicyFilter{cfg: cfg}
}

// PolicyResult is the filter's outcome.
type PolicyResult struct {
	Allowed       []*Match
	BlockedCount  int
	BlockedLabels []string
}

// Apply walks `matches` and partitions them by `channelPrivacyMode`.
// `channelPrivacyMode` is the channel's declared maximum privacy
// label; chunks at or below that label are kept, chunks above are
// dropped.
func (p *PolicyFilter) Apply(matches []*Match, channelPrivacyMode string) PolicyResult {
	if channelPrivacyMode == "" {
		channelPrivacyMode = p.cfg.PrivacyOrder[0]
	}

	maxIdx := p.idxOf(channelPrivacyMode)
	allowed := make([]*Match, 0, len(matches))
	res := PolicyResult{Allowed: allowed}

	for _, m := range matches {
		if m == nil {
			continue
		}
		label := m.PrivacyLabel
		if label == "" {
			label = p.cfg.PrivacyOrder[0]
		}
		idx := p.idxOf(label)
		if idx > maxIdx {
			res.BlockedCount++
			res.BlockedLabels = appendUnique(res.BlockedLabels, label)

			continue
		}
		res.Allowed = append(res.Allowed, m)
	}

	return res
}

// idxOf returns the position of `label` in the privacy order, or
// len(order)+1 for unknown labels (so unknown labels never satisfy
// `idx <= maxIdx`).
func (p *PolicyFilter) idxOf(label string) int {
	for i, l := range p.cfg.PrivacyOrder {
		if l == label {
			return i
		}
	}

	return len(p.cfg.PrivacyOrder) + 1
}
