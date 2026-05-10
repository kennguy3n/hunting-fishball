package eval

import (
	"context"
	"errors"
	"testing"
)

type fakeRetriever struct {
	resp map[string][]RetrieveHit
	err  map[string]error
}

func (f fakeRetriever) Retrieve(_ context.Context, req RetrieveRequest) ([]RetrieveHit, error) {
	if e, ok := f.err[req.Query]; ok {
		return nil, e
	}
	return f.resp[req.Query], nil
}

func TestRun_ScoresEachCase(t *testing.T) {
	t.Parallel()
	r := fakeRetriever{
		resp: map[string][]RetrieveHit{
			"q1": {{ID: "a", Score: 0.9}, {ID: "x", Score: 0.5}},
			"q2": {{ID: "b", Score: 0.8}, {ID: "c", Score: 0.4}},
		},
	}
	suite := EvalSuite{
		Name:     "smoke",
		TenantID: "tenant-a",
		DefaultK: 2,
		Cases: []EvalCase{
			{Query: "q1", ExpectedChunkIDs: []string{"a"}},
			{Query: "q2", ExpectedChunkIDs: []string{"b", "c"}},
		},
	}
	rep, err := Run(context.Background(), r, suite)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Cases) != 2 {
		t.Fatalf("cases: %d", len(rep.Cases))
	}
	if rep.Cases[0].Metrics.MRR != 1 {
		t.Fatalf("q1 MRR: %v", rep.Cases[0].Metrics.MRR)
	}
	if rep.Cases[1].Metrics.RecallAtK != 1 {
		t.Fatalf("q2 recall: %v", rep.Cases[1].Metrics.RecallAtK)
	}
	if rep.Aggregate.PrecisionAtK <= 0 {
		t.Fatalf("aggregate precision: %v", rep.Aggregate.PrecisionAtK)
	}
}

func TestRun_RecordsRetrieverErrors(t *testing.T) {
	t.Parallel()
	r := fakeRetriever{err: map[string]error{"boom": errors.New("backend down")}}
	suite := EvalSuite{
		Name:     "fail",
		TenantID: "tenant-a",
		Cases:    []EvalCase{{Query: "boom", ExpectedChunkIDs: []string{"a"}}},
	}
	rep, err := Run(context.Background(), r, suite)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.FailedCases != 1 {
		t.Fatalf("expected failed=1, got %d", rep.FailedCases)
	}
	if rep.Cases[0].Error == "" {
		t.Fatal("expected error string on case")
	}
}

func TestRun_ExpectedMinScoreGate(t *testing.T) {
	t.Parallel()
	r := fakeRetriever{
		resp: map[string][]RetrieveHit{"q": {{ID: "a", Score: 0.1}}},
	}
	suite := EvalSuite{
		Name:     "score",
		TenantID: "tenant-a",
		Cases: []EvalCase{
			{Query: "q", ExpectedChunkIDs: []string{"a"}, ExpectedMinScore: 0.5},
		},
	}
	rep, err := Run(context.Background(), r, suite)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.FailedCases != 1 {
		t.Fatalf("expected gated case to fail: %+v", rep)
	}
	if rep.Cases[0].Metrics.PrecisionAtK != 0 {
		t.Fatalf("gated case must zero its metrics: %+v", rep.Cases[0])
	}
}

func TestRun_RejectsInvalidSuite(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), fakeRetriever{}, EvalSuite{})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

// TestRun_AggregateExcludesGatedCases is the regression for
// runner.go appending a zero-valued Metrics{} when the
// ExpectedMinScore gate fails, dragging the aggregate down. The
// aggregate must only average over cases that produced real
// metrics (the perfect case here), matching the retriever-error
// path which already excludes its case from aggregation.
func TestRun_AggregateExcludesGatedCases(t *testing.T) {
	t.Parallel()
	r := fakeRetriever{
		resp: map[string][]RetrieveHit{
			"perfect": {{ID: "a", Score: 0.9}},
			"low":     {{ID: "b", Score: 0.1}},
		},
	}
	suite := EvalSuite{
		Name:     "agg",
		TenantID: "tenant-a",
		DefaultK: 1,
		Cases: []EvalCase{
			{Query: "perfect", ExpectedChunkIDs: []string{"a"}},
			{Query: "low", ExpectedChunkIDs: []string{"b"}, ExpectedMinScore: 0.5},
		},
	}
	rep, err := Run(context.Background(), r, suite)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.FailedCases != 1 {
		t.Fatalf("expected one failed case, got %d", rep.FailedCases)
	}
	if rep.Aggregate.PrecisionAtK != 1 {
		t.Fatalf("aggregate precision must equal the perfect case (1.0), got %v", rep.Aggregate.PrecisionAtK)
	}
	if rep.Aggregate.MRR != 1 {
		t.Fatalf("aggregate MRR must equal the perfect case (1.0), got %v", rep.Aggregate.MRR)
	}
}
