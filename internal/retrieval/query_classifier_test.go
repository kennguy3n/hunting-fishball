package retrieval_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

func TestQueryClassifier_Heuristics(t *testing.T) {
	cls := retrieval.NewQueryClassifier(retrieval.ClassifierConfig{})

	cases := []struct {
		name  string
		query string
		want  retrieval.QueryKind
	}{
		{"empty", "", retrieval.QueryKindSemantic},
		{"quotes are exact", `"renew lease"`, retrieval.QueryKindExact},
		{"short lexical is exact", "renew lease", retrieval.QueryKindExact},
		{"question is semantic", "How do I rotate credentials?", retrieval.QueryKindSemantic},
		{"long descriptive is semantic", "describe the rollout plan for the new pipeline", retrieval.QueryKindSemantic},
		{"capitalized noun is entity", "Acme Corp Q4 results", retrieval.QueryKindEntity},
		{"non-question short is exact", "kafka lag alert", retrieval.QueryKindExact},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := cls.Classify(tc.query)
			if got != tc.want {
				t.Fatalf("query %q: got %q want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestQueryClassifier_EmitsPerSourceWeights(t *testing.T) {
	cls := retrieval.NewQueryClassifier(retrieval.ClassifierConfig{})

	_, exactW := cls.Classify(`"exact phrase"`)
	if exactW[retrieval.SourceBM25] <= exactW[retrieval.SourceVector] {
		t.Fatalf("exact must favour BM25, got %+v", exactW)
	}

	_, semW := cls.Classify("How does the embedding cache invalidate keys when a source re-indexes?")
	if semW[retrieval.SourceVector] <= semW[retrieval.SourceBM25] {
		t.Fatalf("semantic must favour vector, got %+v", semW)
	}

	_, entW := cls.Classify("Acme Corp Q4 results")
	if entW[retrieval.SourceGraph] <= entW[retrieval.SourceVector] {
		t.Fatalf("entity must favour graph, got %+v", entW)
	}
}

func TestQueryClassifier_OverrideWeights(t *testing.T) {
	cls := retrieval.NewQueryClassifier(retrieval.ClassifierConfig{
		ExactWeights: map[string]float32{retrieval.SourceBM25: 9, retrieval.SourceVector: 0.1},
	})
	_, w := cls.Classify(`"hot exact"`)
	if w[retrieval.SourceBM25] != 9 || w[retrieval.SourceVector] != 0.1 {
		t.Fatalf("override weights not applied: %+v", w)
	}
}

func TestQueryRoutingEnabled_RespectsEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"1", true}, {"true", true}, {"yes", true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CONTEXT_ENGINE_QUERY_ROUTING_ENABLED", tc.val)
			if got := retrieval.QueryRoutingEnabled(); got != tc.want {
				t.Fatalf("QueryRoutingEnabled(%q): got %v want %v", tc.val, got, tc.want)
			}
		})
	}
}
