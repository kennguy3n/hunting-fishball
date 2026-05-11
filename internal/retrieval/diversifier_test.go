package retrieval

import (
	"context"
	"reflect"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// diversifierFakeVS / diversifierFakeEmb satisfy the narrow
// VectorStore / Embedder contracts NewHandler validates. They're
// kept local to this file (rather than reused from handler_test.go)
// because handler_test.go is in package retrieval_test — the
// default-install assertion has to read Handler.cfg, which is
// unexported, so this test stays in package retrieval.
type diversifierFakeVS struct{}

func (diversifierFakeVS) Search(context.Context, string, []float32, storage.SearchOpts) ([]storage.QdrantHit, error) {
	return nil, nil
}

type diversifierFakeEmb struct{}

func (diversifierFakeEmb) EmbedQuery(context.Context, string, string) ([]float32, error) {
	return []float32{0}, nil
}

// TestNewHandler_InstallsDefaultDiversifier asserts NewHandler
// honours HandlerConfig.Diversifier's doc contract: when nil the
// handler installs an MMRDiversifier with the default Jaccard
// similarity. Without this, callers that wire NewHandler with the
// minimum required dependencies (VectorStore + Embedder) still see
// `req.Diversity > 0` silently ignored at the handler call sites,
// because `h.cfg.Diversifier != nil` short-circuits to false.
func TestNewHandler_InstallsDefaultDiversifier(t *testing.T) {
	h, err := NewHandler(HandlerConfig{
		VectorStore: diversifierFakeVS{},
		Embedder:    diversifierFakeEmb{},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if h.cfg.Diversifier == nil {
		t.Fatal("NewHandler must install a default Diversifier (HandlerConfig.Diversifier doc contract)")
	}
	if _, ok := h.cfg.Diversifier.(*MMRDiversifier); !ok {
		t.Fatalf("default Diversifier must be *MMRDiversifier; got %T", h.cfg.Diversifier)
	}
}

// TestNewHandler_PreservesExplicitDiversifier verifies the
// default-install path doesn't clobber a caller-supplied
// Diversifier. Callers wiring a custom reranker-adjacent diversity
// strategy must retain it.
func TestNewHandler_PreservesExplicitDiversifier(t *testing.T) {
	custom := NewMMRDiversifier(jaccardSimilarity)
	h, err := NewHandler(HandlerConfig{
		VectorStore: diversifierFakeVS{},
		Embedder:    diversifierFakeEmb{},
		Diversifier: custom,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if h.cfg.Diversifier != custom {
		t.Fatalf("NewHandler must keep caller-supplied Diversifier; got %T", h.cfg.Diversifier)
	}
}

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

func TestMMRDiversifier_LambdaOneMaxDiversity(t *testing.T) {
	// With lambda=1 the score reduces to pure diversity. The
	// first pick is the most relevant candidate (maxSim is zero
	// when nothing has been selected yet, so all candidates tie
	// on score 0 and the input-order winner is picked first).
	// From then on the algorithm greedily prefers the least-
	// similar candidate regardless of its relevance score —
	// matching the public API contract on RetrieveRequest.Diversity.
	d := NewMMRDiversifier(nil)
	in := []*Match{
		mkMatch("a", "alpha beta gamma", 1.0),
		mkMatch("b", "alpha beta gamma delta", 0.95), // similar to a
		mkMatch("c", "totally unrelated rocket science", 0.3),
	}
	out := d.Diversify(context.Background(), in, 1, 0)
	got := ids(out)
	if got[0] != "a" {
		t.Fatalf("lambda=1 first pick must be highest-relevance (tie-break); got order %v", got)
	}
	if got[1] != "c" {
		t.Fatalf("lambda=1 second pick must be the most-dissimilar candidate; got order %v", got)
	}
}

func TestMMRDiversifier_HighLambdaPrefersDiverse(t *testing.T) {
	// Two highly similar high-relevance items, one diverse low-
	// relevance item. With lambda close to 1 the diversifier
	// promotes the diverse item ahead of the near-duplicate,
	// because the duplicate's MMR penalty (−lambda·maxSim)
	// outweighs its relevance bump.
	d := NewMMRDiversifier(nil)
	in := []*Match{
		mkMatch("a", "the quick brown fox jumps over", 1.0),
		mkMatch("b", "the quick brown fox jumps over again", 0.95),
		mkMatch("c", "totally unrelated rocket science topic", 0.3),
	}
	out := d.Diversify(context.Background(), in, 0.7, 0)
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

func TestMMRDiversifier_LowLambdaPrefersRelevance(t *testing.T) {
	// Mirror of the high-lambda case: with lambda near 0 the
	// relevance term dominates so the near-duplicate (b) is
	// picked second despite its similarity to a.
	d := NewMMRDiversifier(nil)
	in := []*Match{
		mkMatch("a", "the quick brown fox jumps over", 1.0),
		mkMatch("b", "the quick brown fox jumps over again", 0.95),
		mkMatch("c", "totally unrelated rocket science topic", 0.3),
	}
	out := d.Diversify(context.Background(), in, 0.1, 0)
	got := ids(out)
	if got[0] != "a" {
		t.Fatalf("first pick must be highest-relevance; got %v", got)
	}
	if got[1] != "b" {
		t.Fatalf("low lambda should pick the next-most-relevant candidate second; got order %v", got)
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
