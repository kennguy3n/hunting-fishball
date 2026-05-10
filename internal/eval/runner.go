package eval

import (
	"context"
	"errors"
	"time"
)

// RetrieveRequest is the runner's neutral retrieval input. We keep
// it here (instead of importing internal/retrieval) so the eval
// package stays a leaf — handlers can adapt the public retrieval
// envelope into this shape via the Retriever interface.
type RetrieveRequest struct {
	TenantID    string
	Query       string
	TopK        int
	SkillID     string
	PrivacyMode string
}

// RetrieveHit is the minimum surface the runner needs from a hit:
// its chunk ID and score. Adapters lift these out of the richer
// retrieval.RetrieveHit shape.
type RetrieveHit struct {
	ID    string
	Score float32
}

// Retriever is the dependency the runner drives. The retrieval
// package's Handler is wired through retrievalAdapter (see
// handler.go) so production paths use the live retrieval stack.
type Retriever interface {
	Retrieve(ctx context.Context, req RetrieveRequest) ([]RetrieveHit, error)
}

// Run executes every case in the suite against the retriever and
// returns a populated EvalReport. The runner does not parallelise —
// retrieval results may be cache-keyed by recency, and in any case
// admin-suite latency is dwarfed by retrieval itself, so a serial
// driver gives more predictable measurements.
//
// Per-case errors are recorded on the EvalCaseResult.Error field
// and bumped onto report.FailedCases without aborting the run; only
// suite-level invariants (Validate failure) cause the function to
// return an error.
func Run(ctx context.Context, r Retriever, suite EvalSuite) (*EvalReport, error) {
	if r == nil {
		return nil, errors.New("eval: Run requires a Retriever")
	}
	if err := suite.Validate(); err != nil {
		return nil, err
	}

	startedAt := time.Now().UTC()
	report := &EvalReport{
		SuiteName: suite.Name,
		TenantID:  suite.TenantID,
		StartedAt: startedAt,
		K:         suite.DefaultK,
		Cases:     make([]EvalCaseResult, 0, len(suite.Cases)),
	}
	if report.K == 0 {
		report.K = defaultK
	}

	scoredMetrics := make([]Metrics, 0, len(suite.Cases))

	for _, c := range suite.Cases {
		k := suite.EffectiveK(c)
		caseStart := time.Now()
		hits, err := r.Retrieve(ctx, RetrieveRequest{
			TenantID:    suite.TenantID,
			Query:       c.Query,
			TopK:        k,
			SkillID:     c.SkillID,
			PrivacyMode: c.PrivacyMode,
		})
		caseDuration := time.Since(caseStart)

		ids := make([]string, 0, len(hits))
		var topScore float32
		for i, h := range hits {
			ids = append(ids, h.ID)
			if i == 0 {
				topScore = h.Score
			}
		}

		caseResult := EvalCaseResult{
			Query:    c.Query,
			Expected: append([]string(nil), c.ExpectedChunkIDs...),
			Returned: ids,
			K:        k,
			TopScore: topScore,
			Duration: caseDuration,
		}

		if err != nil {
			caseResult.Error = err.Error()
			report.FailedCases++
			report.Cases = append(report.Cases, caseResult)
			continue
		}

		// ExpectedMinScore gates the case as "scored" — when the
		// caller wants to assert on the absolute relevance of the
		// top hit, a miss zeroes the metrics. We still record the
		// returned IDs so operators can see the regression.
		if c.ExpectedMinScore > 0 && topScore < c.ExpectedMinScore {
			caseResult.Error = "top score below expected_min_score"
			scoredMetrics = append(scoredMetrics, Metrics{})
			report.FailedCases++
			report.Cases = append(report.Cases, caseResult)
			continue
		}

		caseResult.Metrics = ComputeMetrics(ids, c.ExpectedChunkIDs, k)
		scoredMetrics = append(scoredMetrics, caseResult.Metrics)
		report.Cases = append(report.Cases, caseResult)

		if err := ctx.Err(); err != nil {
			report.CompletedAt = time.Now().UTC()
			report.Duration = report.CompletedAt.Sub(report.StartedAt)
			return report, err
		}
	}

	report.Aggregate = AggregateMetrics(scoredMetrics)
	report.CompletedAt = time.Now().UTC()
	report.Duration = report.CompletedAt.Sub(report.StartedAt)
	return report, nil
}
