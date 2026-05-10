// feedback.go — Round-5 Task 16.
//
// The eval runner's ground-truth comes from two sources:
//
//  1. The curated golden corpus loaded from
//     tests/eval/golden_corpus.json (Task 1).
//
//  2. Live retrieval feedback collected via
//     POST /v1/retrieve/feedback (Task 16). Every
//     thumbs-up/thumbs-down lands in feedback_events; the eval
//     runner aggregates those rows by query_id, projects them
//     onto EvalCases, and merges them with the curated corpus
//     so regression-gate metrics adapt to real-world traffic.
//
// We keep the integration in this package — not in
// internal/retrieval — so eval stays a leaf and tests can drive
// it with a tiny stub FeedbackProvider instead of standing up
// the full retrieval HTTP stack.
package eval

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// FeedbackRow is the per-(query_id, chunk_id) signal the eval
// runner consumes. Mirrors retrieval.FeedbackEvent's relevant
// columns so the adapter is a one-line projection.
type FeedbackRow struct {
	TenantID string
	QueryID  string
	Query    string
	ChunkID  string
	Relevant bool
	SkillID  string
}

// FeedbackProvider is the contract the eval runner uses to pull
// labelled feedback. The retrieval feedback repository
// implements this via a thin adapter so eval doesn't import
// retrieval.
type FeedbackProvider interface {
	ListByTenantForEval(ctx context.Context, tenantID string) ([]FeedbackRow, error)
}

// CasesFromFeedback aggregates rows by query_id and returns one
// EvalCase per query. ExpectedChunkIDs is the deduplicated set of
// chunks that received a positive (relevant=true) signal — a
// negative signal does not bump a chunk into the expected set
// but does prevent ambiguous queries (where the only signal is
// thumbs-down) from being treated as having ground-truth.
//
// The returned cases are sorted by query_id (deterministic
// ordering simplifies regression diffs) and the Query field is
// taken from the most recent row whose Query is non-empty so
// admins see the exact query text that drove the signal.
func CasesFromFeedback(rows []FeedbackRow) []EvalCase {
	if len(rows) == 0 {
		return nil
	}
	type bucket struct {
		query    string
		skill    string
		expected map[string]struct{}
		anyPos   bool
	}
	byQuery := make(map[string]*bucket)
	for _, r := range rows {
		qid := strings.TrimSpace(r.QueryID)
		if qid == "" {
			continue
		}
		b, ok := byQuery[qid]
		if !ok {
			b = &bucket{
				expected: make(map[string]struct{}),
			}
			byQuery[qid] = b
		}
		if strings.TrimSpace(r.Query) != "" {
			b.query = r.Query
		}
		if r.SkillID != "" {
			b.skill = r.SkillID
		}
		if r.Relevant {
			b.anyPos = true
			if r.ChunkID != "" {
				b.expected[r.ChunkID] = struct{}{}
			}
		}
	}

	keys := make([]string, 0, len(byQuery))
	for k, b := range byQuery {
		// Drop queries that received only negative feedback —
		// without a positive signal we have no ground-truth and
		// scoring would falsely zero-out the metric.
		if !b.anyPos {
			continue
		}
		if b.query == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	cases := make([]EvalCase, 0, len(keys))
	for _, k := range keys {
		b := byQuery[k]
		expected := make([]string, 0, len(b.expected))
		for cid := range b.expected {
			expected = append(expected, cid)
		}
		sort.Strings(expected)
		cases = append(cases, EvalCase{
			Query:            b.query,
			ExpectedChunkIDs: expected,
			SkillID:          b.skill,
		})
	}
	return cases
}

// AugmentSuiteWithFeedback appends feedback-derived cases onto
// suite.Cases. The merge is intentionally additive — a curated
// case stays in the suite even if a feedback row contradicts it
// because the curated corpus is the canonical source-of-truth.
func AugmentSuiteWithFeedback(ctx context.Context, p FeedbackProvider, suite *EvalSuite) error {
	if p == nil {
		return errors.New("eval: AugmentSuiteWithFeedback requires a FeedbackProvider")
	}
	if suite == nil {
		return errors.New("eval: AugmentSuiteWithFeedback requires a suite")
	}
	if suite.TenantID == "" {
		return errors.New("eval: AugmentSuiteWithFeedback requires suite.TenantID")
	}
	rows, err := p.ListByTenantForEval(ctx, suite.TenantID)
	if err != nil {
		return err
	}
	cases := CasesFromFeedback(rows)
	if len(cases) == 0 {
		return nil
	}
	suite.Cases = append(suite.Cases, cases...)
	return nil
}
