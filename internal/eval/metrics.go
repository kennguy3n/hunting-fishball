package eval

import "math"

// Metrics is the four-number summary the runner emits per case and
// per suite. All values are in [0, 1] except where noted.
//
//   - PrecisionAtK: |relevant ∩ retrieved@K| / K
//   - RecallAtK:    |relevant ∩ retrieved@K| / |relevant|
//   - MRR:          1 / rank-of-first-relevant; 0 when no match in @K
//   - NDCG:         normalised discounted cumulative gain @K with
//     binary relevance (1 if expected, 0 otherwise)
type Metrics struct {
	PrecisionAtK float64 `json:"precision_at_k"`
	RecallAtK    float64 `json:"recall_at_k"`
	MRR          float64 `json:"mrr"`
	NDCG         float64 `json:"ndcg"`
}

// ComputeMetrics scores `retrievedIDs` (in rank order) against the
// `expectedIDs` ground truth, truncating at K.
//
// When K <= 0 the function uses len(retrievedIDs) so callers that
// don't care about a fixed K still get a meaningful score. When the
// expected set is empty, retrieval is vacuously correct: precision
// is 0 (no relevant retrieved is possible), recall is 1 (nothing to
// miss), and ranking metrics are 0.
func ComputeMetrics(retrievedIDs, expectedIDs []string, k int) Metrics {
	if k <= 0 || k > len(retrievedIDs) {
		k = len(retrievedIDs)
	}
	if len(expectedIDs) == 0 {
		return Metrics{RecallAtK: 1}
	}

	expectedSet := make(map[string]struct{}, len(expectedIDs))
	for _, id := range expectedIDs {
		expectedSet[id] = struct{}{}
	}

	var hits int
	mrr := 0.0
	dcg := 0.0
	for i := 0; i < k; i++ {
		id := retrievedIDs[i]
		if _, ok := expectedSet[id]; !ok {
			continue
		}
		hits++
		if mrr == 0 {
			mrr = 1.0 / float64(i+1)
		}
		// log2(i+2) so the first position has weight 1/log2(2)=1.
		dcg += 1.0 / math.Log2(float64(i+2))
	}

	idcg := idealDCG(min(k, len(expectedIDs)))
	ndcg := 0.0
	if idcg > 0 {
		ndcg = dcg / idcg
	}

	precision := 0.0
	if k > 0 {
		precision = float64(hits) / float64(k)
	}

	return Metrics{
		PrecisionAtK: precision,
		RecallAtK:    float64(hits) / float64(len(expectedIDs)),
		MRR:          mrr,
		NDCG:         ndcg,
	}
}

// idealDCG returns the DCG@k for an ideal ranking of binary-relevance
// labels: every relevant item appears at the top of the list.
func idealDCG(k int) float64 {
	if k <= 0 {
		return 0
	}
	v := 0.0
	for i := 0; i < k; i++ {
		v += 1.0 / math.Log2(float64(i+2))
	}
	return v
}

// AggregateMetrics averages each metric across the supplied list,
// skipping zero-valued slots. The runner uses this to roll case
// results into the EvalReport's Aggregate field.
//
// "Average over scored cases" matches the convention used by IR
// benchmarks (BEIR, MTEB) — failed cases are reported separately so
// they don't quietly drag the mean.
func AggregateMetrics(items []Metrics) Metrics {
	if len(items) == 0 {
		return Metrics{}
	}
	var (
		p, r, m, n float64
		count      float64
	)
	for _, it := range items {
		count++
		p += it.PrecisionAtK
		r += it.RecallAtK
		m += it.MRR
		n += it.NDCG
	}
	if count == 0 {
		return Metrics{}
	}
	return Metrics{
		PrecisionAtK: p / count,
		RecallAtK:    r / count,
		MRR:          m / count,
		NDCG:         n / count,
	}
}

// min is Go 1.21+ builtin but explicit here for readability.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
