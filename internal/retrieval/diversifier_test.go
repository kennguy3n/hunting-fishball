package retrieval

import (
	"context"
	"reflect"
	"testing"
)

func mkMatch(id, text string, score float32) *Match {
	return &Match{ID: id, Text: text, Score: score}
}

func TestMMRDiversifier_LambdaZeroPassthrough(t *testing.T) {
	d := NewMMRDiversifier(nil)
	in := []*Match{
		mkMatch("a", "hello world", 1.0),
		mkMatch("b", "hello world again", 0.9),
		mkMatch("c", "completely different topic", 0.8),
	}
	out := d.Diversify(context.Background(), in, 0, 0)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("lambda=0 must passthrough unchanged; got order %v", ids(out))
	}
}

func TestMMRDiversifier_LambdaOnePicksRelevanceFirst(t *testing.T) {
	// With lambda=1 the score reduces to pure relevance — the
	// algorithm picks descending by Match.Score regardless of
	// similarity. We still expect the order [a, b, c] but the
	// diversifier must walk every candidate (no passthrough).
	d := NewMMRDiversifier(nil)
	in := []*Match{
		mkMatch("a", "alpha", 1.0),
		mkMatch("b", "alpha beta", 0.9),
		mkMatch("c", "beta gamma", 0.8),
	}
	out := d.Diversify(context.Background(), in, 1, 0)
	if got, want := ids(out), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lambda=1 expected %v, got %v", want, got)
	}
}

func TestMMRDiversifier_MidLambdaPrefersDiverse(t *testing.T) {
	// Two highly similar high-relevance items, one diverse low-
	// relevance item. With lambda < 1 the diversifier should
	// promote the diverse item ahead of the near-duplicate,
	// because the duplicate's MMR penalty pushes it below the
	// diverse candidate.
	d := NewMMRDiversifier(nil)
	in := []*Match{
		mkMatch("a", "the quick brown fox jumps over", 1.0),
		mkMatch("b", "the quick brown fox jumps over again", 0.95),
		mkMatch("c", "totally unrelated rocket science topic", 0.3),
	}
	out := d.Diversify(context.Background(), in, 0.3, 0)
	got := ids(out)
	// First pick is always the most relevant.
	if got[0] != "a" {
		t.Fatalf("first pick must be highest-relevance; got %v", got)
	}
	// Second pick should be the diverse candidate, not the near-
	// duplicate of the first.
	if got[1] != "c" {
		t.Fatalf("expected diverse pick second; got order %v", got)
	}
}

func TestMMRDiversifier_TopKLimit(t *testing.T) {
	d := NewMMRDiversifier(nil)
	in := []*Match{
		mkMatch("a", "alpha", 1.0),
		mkMatch("b", "beta", 0.9),
		mkMatch("c", "gamma", 0.8),
		mkMatch("d", "delta", 0.7),
	}
	out := d.Diversify(context.Background(), in, 0.5, 2)
	if len(out) != 2 {
		t.Fatalf("topK=2 must cap output; got %d", len(out))
	}
}

func TestJaccardSimilarity(t *testing.T) {
	cases := []struct {
		name   string
		a, b   *Match
		expect float32
	}{
		{"identical", mkMatch("x", "alpha beta", 0), mkMatch("x", "alpha beta", 0), 1},
		{"disjoint", mkMatch("a", "alpha", 0), mkMatch("b", "beta", 0), 0},
		{"half overlap", mkMatch("a", "alpha beta", 0), mkMatch("b", "alpha gamma", 0), 1.0 / 3.0},
		{"nil-safe", nil, mkMatch("b", "x", 0), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jaccardSimilarity(tc.a, tc.b)
			if !approx(got, tc.expect, 1e-6) {
				t.Fatalf("got %v want %v", got, tc.expect)
			}
		})
	}
}

func TestNormalisedScores_AllEqual(t *testing.T) {
	in := []*Match{mkMatch("a", "x", 0.5), mkMatch("b", "y", 0.5)}
	got := normalisedScores(in)
	for _, v := range got {
		if v != 1 {
			t.Fatalf("equal scores should normalise to 1; got %v", got)
		}
	}
}

func ids(matches []*Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.ID)
	}
	return out
}

func approx(a, b, tol float32) bool {
	if a > b {
		return a-b <= tol
	}
	return b-a <= tol
}
