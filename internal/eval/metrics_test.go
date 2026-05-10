package eval

import (
	"math"
	"testing"
)

func TestComputeMetrics_PerfectRetrieval(t *testing.T) {
	t.Parallel()
	got := ComputeMetrics([]string{"a", "b", "c"}, []string{"a", "b", "c"}, 3)
	if got.PrecisionAtK != 1 || got.RecallAtK != 1 || got.MRR != 1 {
		t.Fatalf("perfect retrieval: %+v", got)
	}
	if math.Abs(got.NDCG-1) > 1e-9 {
		t.Fatalf("perfect NDCG: %v", got.NDCG)
	}
}

func TestComputeMetrics_NoRelevant(t *testing.T) {
	t.Parallel()
	got := ComputeMetrics([]string{"x", "y", "z"}, []string{"a", "b"}, 3)
	if got.PrecisionAtK != 0 || got.RecallAtK != 0 || got.MRR != 0 || got.NDCG != 0 {
		t.Fatalf("no relevant: %+v", got)
	}
}

func TestComputeMetrics_RankPositionMatters(t *testing.T) {
	t.Parallel()
	expected := []string{"rel"}
	first := ComputeMetrics([]string{"rel", "x", "y"}, expected, 3)
	last := ComputeMetrics([]string{"x", "y", "rel"}, expected, 3)
	if first.MRR <= last.MRR {
		t.Fatalf("MRR should reward earlier rank: first=%v last=%v", first.MRR, last.MRR)
	}
	if first.NDCG <= last.NDCG {
		t.Fatalf("NDCG should reward earlier rank: first=%v last=%v", first.NDCG, last.NDCG)
	}
	// Recall@K only cares about the set, not the rank.
	if first.RecallAtK != last.RecallAtK {
		t.Fatalf("recall changed with rank: first=%v last=%v", first.RecallAtK, last.RecallAtK)
	}
}

func TestComputeMetrics_PrecisionAtK_PartialRelevant(t *testing.T) {
	t.Parallel()
	got := ComputeMetrics([]string{"a", "x", "b", "y"}, []string{"a", "b", "c"}, 4)
	wantP := 0.5  // 2 hits / 4
	wantR := 2.0 / 3.0
	if math.Abs(got.PrecisionAtK-wantP) > 1e-9 {
		t.Fatalf("precision: %v want %v", got.PrecisionAtK, wantP)
	}
	if math.Abs(got.RecallAtK-wantR) > 1e-9 {
		t.Fatalf("recall: %v want %v", got.RecallAtK, wantR)
	}
}

func TestComputeMetrics_KOverflow(t *testing.T) {
	t.Parallel()
	// K larger than retrieved length should clamp to len.
	got := ComputeMetrics([]string{"a", "b"}, []string{"a"}, 99)
	if got.RecallAtK != 1 {
		t.Fatalf("recall should be 1: %+v", got)
	}
}

func TestComputeMetrics_EmptyExpected(t *testing.T) {
	t.Parallel()
	got := ComputeMetrics([]string{"a"}, nil, 1)
	// Vacuous: nothing to retrieve, recall is 1, ranking metrics 0.
	if got.RecallAtK != 1 {
		t.Fatalf("vacuous recall: %+v", got)
	}
}

func TestAggregateMetrics_Average(t *testing.T) {
	t.Parallel()
	in := []Metrics{
		{PrecisionAtK: 1, RecallAtK: 1, MRR: 1, NDCG: 1},
		{PrecisionAtK: 0, RecallAtK: 0, MRR: 0, NDCG: 0},
		{PrecisionAtK: 0.5, RecallAtK: 0.5, MRR: 0.5, NDCG: 0.5},
	}
	got := AggregateMetrics(in)
	want := 1.5 / 3.0
	if math.Abs(got.PrecisionAtK-want) > 1e-9 {
		t.Fatalf("precision avg: %v want %v", got.PrecisionAtK, want)
	}
}

func TestAggregateMetrics_Empty(t *testing.T) {
	t.Parallel()
	got := AggregateMetrics(nil)
	if got != (Metrics{}) {
		t.Fatalf("empty agg: %+v", got)
	}
}
