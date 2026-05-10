package eval_test

import (
	"context"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/eval"
)

type stubFeedbackProvider struct {
	rows []eval.FeedbackRow
	err  error
}

func (s *stubFeedbackProvider) ListByTenantForEval(_ context.Context, _ string) ([]eval.FeedbackRow, error) {
	return s.rows, s.err
}

func TestCasesFromFeedback_AggregatesByQuery(t *testing.T) {
	t.Parallel()
	rows := []eval.FeedbackRow{
		{TenantID: "tenant-a", QueryID: "q1", Query: "what is X?", ChunkID: "c1", Relevant: true},
		{TenantID: "tenant-a", QueryID: "q1", Query: "what is X?", ChunkID: "c2", Relevant: true},
		{TenantID: "tenant-a", QueryID: "q1", Query: "what is X?", ChunkID: "c3", Relevant: false},
		{TenantID: "tenant-a", QueryID: "q2", Query: "where is Y?", ChunkID: "c9", Relevant: true},
	}
	cases := eval.CasesFromFeedback(rows)
	if len(cases) != 2 {
		t.Fatalf("expected 2 cases, got %d", len(cases))
	}
	// Cases are sorted by query_id, so q1 first.
	if cases[0].Query != "what is X?" {
		t.Fatalf("case[0] query: %q", cases[0].Query)
	}
	if len(cases[0].ExpectedChunkIDs) != 2 {
		t.Fatalf("case[0] expected: %v", cases[0].ExpectedChunkIDs)
	}
	if cases[1].Query != "where is Y?" {
		t.Fatalf("case[1] query: %q", cases[1].Query)
	}
}

func TestCasesFromFeedback_DropsAllNegative(t *testing.T) {
	t.Parallel()
	rows := []eval.FeedbackRow{
		{TenantID: "tenant-a", QueryID: "q1", Query: "ambiguous", ChunkID: "c", Relevant: false},
		{TenantID: "tenant-a", QueryID: "q1", Query: "ambiguous", ChunkID: "c2", Relevant: false},
	}
	cases := eval.CasesFromFeedback(rows)
	if len(cases) != 0 {
		t.Fatalf("expected 0 cases (no positive signal), got %d", len(cases))
	}
}

func TestCasesFromFeedback_EmptyInput(t *testing.T) {
	t.Parallel()
	if got := eval.CasesFromFeedback(nil); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestCasesFromFeedback_DropsEmptyQueryID(t *testing.T) {
	t.Parallel()
	rows := []eval.FeedbackRow{
		{TenantID: "tenant-a", QueryID: "", Query: "q", ChunkID: "c", Relevant: true},
	}
	if got := eval.CasesFromFeedback(rows); len(got) != 0 {
		t.Fatalf("expected drop, got %d", len(got))
	}
}

func TestAugmentSuiteWithFeedback_AppendsCases(t *testing.T) {
	t.Parallel()
	provider := &stubFeedbackProvider{
		rows: []eval.FeedbackRow{
			{TenantID: "tenant-a", QueryID: "fb1", Query: "feedback case", ChunkID: "fb-c", Relevant: true},
		},
	}
	suite := &eval.EvalSuite{
		Name: "test", TenantID: "tenant-a",
		Cases: []eval.EvalCase{{Query: "curated", ExpectedChunkIDs: []string{"x"}}},
	}
	if err := eval.AugmentSuiteWithFeedback(context.Background(), provider, suite); err != nil {
		t.Fatalf("AugmentSuiteWithFeedback: %v", err)
	}
	if len(suite.Cases) != 2 {
		t.Fatalf("expected 2 cases after augmentation, got %d", len(suite.Cases))
	}
	if suite.Cases[0].Query != "curated" {
		t.Fatalf("curated case overwritten: %q", suite.Cases[0].Query)
	}
	if suite.Cases[1].Query != "feedback case" {
		t.Fatalf("feedback case missing: %q", suite.Cases[1].Query)
	}
}

func TestAugmentSuiteWithFeedback_NilProviderErrors(t *testing.T) {
	t.Parallel()
	if err := eval.AugmentSuiteWithFeedback(context.Background(), nil, &eval.EvalSuite{}); err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestAugmentSuiteWithFeedback_MissingTenantErrors(t *testing.T) {
	t.Parallel()
	provider := &stubFeedbackProvider{}
	if err := eval.AugmentSuiteWithFeedback(context.Background(), provider, &eval.EvalSuite{}); err == nil {
		t.Fatal("expected error for missing tenant")
	}
}
