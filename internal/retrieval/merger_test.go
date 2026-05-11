package retrieval_test

import (
	"math"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

func TestMerger_RRF_FormulaSingleStream(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{K: 60})
	stream := []*retrieval.Match{
		{ID: "a", Source: retrieval.SourceVector},
		{ID: "b", Source: retrieval.SourceVector},
		{ID: "c", Source: retrieval.SourceVector},
	}
	merged := merger.Merge(stream)
	if len(merged) != 3 {
		t.Fatalf("want 3 hits, got %d", len(merged))
	}
	expected := []float64{1.0 / 61, 1.0 / 62, 1.0 / 63}
	want := []string{"a", "b", "c"}
	for i, m := range merged {
		if m.ID != want[i] {
			t.Fatalf("merged[%d].ID = %q; want %q", i, m.ID, want[i])
		}
		if math.Abs(float64(m.Score)-expected[i]) > 1e-6 {
			t.Fatalf("merged[%d].Score = %v; want %v", i, m.Score, expected[i])
		}
		if len(m.Sources) != 1 || m.Sources[0] != retrieval.SourceVector {
			t.Fatalf("Sources: %v", m.Sources)
		}
	}
}

func TestMerger_RRF_MultiStreamSum(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{K: 60})
	vector := []*retrieval.Match{
		{ID: "a", Source: retrieval.SourceVector},
		{ID: "b", Source: retrieval.SourceVector},
	}
	bm25 := []*retrieval.Match{
		{ID: "b", Source: retrieval.SourceBM25},
		{ID: "a", Source: retrieval.SourceBM25},
	}
	merged := merger.Merge(vector, bm25)
	if len(merged) != 2 {
		t.Fatalf("merged: %d", len(merged))
	}

	scoreFor := map[string]float32{}
	sourcesFor := map[string][]string{}
	for _, m := range merged {
		scoreFor[m.ID] = m.Score
		sourcesFor[m.ID] = m.Sources
	}
	wantA := float32(1.0/61 + 1.0/62)
	wantB := float32(1.0/62 + 1.0/61)
	if math.Abs(float64(scoreFor["a"]-wantA)) > 1e-6 || math.Abs(float64(scoreFor["b"]-wantB)) > 1e-6 {
		t.Fatalf("scores: a=%v want %v ; b=%v want %v", scoreFor["a"], wantA, scoreFor["b"], wantB)
	}
	if len(sourcesFor["a"]) != 2 || len(sourcesFor["b"]) != 2 {
		t.Fatalf("sources: a=%v b=%v", sourcesFor["a"], sourcesFor["b"])
	}
}

func TestMerger_KOverride(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{K: 0}) // defaults to 60
	one := merger.Merge([]*retrieval.Match{{ID: "a", Source: retrieval.SourceVector}})
	if math.Abs(float64(one[0].Score-float32(1.0/61))) > 1e-6 {
		t.Fatalf("k=60 default broke: %v", one[0].Score)
	}

	mergerK10 := retrieval.NewMerger(retrieval.MergerConfig{K: 10})
	one = mergerK10.Merge([]*retrieval.Match{{ID: "a", Source: retrieval.SourceVector}})
	if math.Abs(float64(one[0].Score-float32(1.0/11))) > 1e-6 {
		t.Fatalf("k=10 broke: %v", one[0].Score)
	}
}

func TestMerger_PerSourceWeight(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{
		K: 60,
		PerSourceWeight: map[string]float32{
			retrieval.SourceVector: 2,
			retrieval.SourceBM25:   0,
		},
	})
	merged := merger.Merge(
		[]*retrieval.Match{{ID: "a", Source: retrieval.SourceVector}},
		[]*retrieval.Match{{ID: "a", Source: retrieval.SourceBM25}},
	)
	if len(merged) != 1 {
		t.Fatalf("merged: %d", len(merged))
	}
	want := float32(2.0 / 61)
	if math.Abs(float64(merged[0].Score-want)) > 1e-6 {
		t.Fatalf("weighted score: %v want %v", merged[0].Score, want)
	}
}

func TestMerger_DropsNilAndEmpty(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{})
	merged := merger.Merge([]*retrieval.Match{
		nil,
		{ID: "", Source: retrieval.SourceVector},
		{ID: "a", Source: retrieval.SourceVector},
	})
	if len(merged) != 1 || merged[0].ID != "a" {
		t.Fatalf("merged: %+v", merged)
	}
}

func TestMerger_PreservesProvenanceFromAnyStream(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{})
	now := time.Now()
	merged := merger.Merge(
		[]*retrieval.Match{{ID: "a", Source: retrieval.SourceVector, Title: "from-vector", IngestedAt: now}},
		[]*retrieval.Match{{ID: "a", Source: retrieval.SourceBM25, URI: "from-bm25"}},
	)
	if len(merged) != 1 {
		t.Fatalf("merged: %d", len(merged))
	}
	if merged[0].Title != "from-vector" || merged[0].URI != "from-bm25" {
		t.Fatalf("provenance: %+v", merged[0])
	}
	if merged[0].IngestedAt != now {
		t.Fatalf("ingestedAt: %v", merged[0].IngestedAt)
	}
}

// TestMerger_PreservesBM25OriginalScore guards the contract relied on
// by the reranker: even though Merge zeroes Score and rewrites it with
// the RRF sum, the BM25 stream's source-specific score is preserved
// on Match.OriginalScore so BM25Weight can blend in the actual
// lexical signal rather than the fused score.
func TestMerger_PreservesBM25OriginalScore(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{K: 60})
	merged := merger.Merge(
		[]*retrieval.Match{{ID: "a", Source: retrieval.SourceVector, Score: 0.95}},
		[]*retrieval.Match{{ID: "a", Source: retrieval.SourceBM25, Score: 7.5, OriginalScore: 7.5}},
	)
	if len(merged) != 1 {
		t.Fatalf("merged: %d", len(merged))
	}
	if math.Abs(float64(merged[0].OriginalScore-7.5)) > 1e-6 {
		t.Fatalf("OriginalScore = %v; want 7.5", merged[0].OriginalScore)
	}
	// Score must reflect the RRF sum (1/61 + 1/61 = 2/61) — i.e.,
	// OriginalScore is preserved separately, not folded into Score.
	want := float32(2.0 / 61)
	if math.Abs(float64(merged[0].Score-want)) > 1e-5 {
		t.Fatalf("Score = %v; want %v", merged[0].Score, want)
	}
}

func TestMerger_StableTieBreaking(t *testing.T) {
	t.Parallel()

	merger := retrieval.NewMerger(retrieval.MergerConfig{})
	merged := merger.Merge([]*retrieval.Match{
		{ID: "z", Source: retrieval.SourceVector, Rank: 1},
		{ID: "a", Source: retrieval.SourceVector, Rank: 1},
	})
	if len(merged) != 2 {
		t.Fatalf("merged: %d", len(merged))
	}
	// Both share rank 1 → equal RRF → tie break by ID asc.
	if merged[0].ID != "a" || merged[1].ID != "z" {
		t.Fatalf("tie order: %+v", []string{merged[0].ID, merged[1].ID})
	}
}

// TestDedup_OverlappingChunkIDsFromTwoBackends — Round-9 Task 6.
// The merger already collapses by ID inside Merge, but Dedup is the
// defensive post-merge pass that runs between Merge and Rerank in
// the handler. It must collapse duplicate chunk_ids regardless of
// whether they came from one stream or two, keep the higher-scored
// entry, and union the Sources lists.
func TestDedup_OverlappingChunkIDsFromTwoBackends(t *testing.T) {
	t.Parallel()
	// Simulate a post-merge stream that, due to some downstream
	// splice (e.g. pinned-results adapter), reintroduces a
	// duplicate of "shared" with a lower score from BM25 after the
	// vector-sourced winner was already emitted.
	input := []*retrieval.Match{
		{ID: "shared", Score: 0.8, Sources: []string{retrieval.SourceVector}},
		{ID: "only-vec", Score: 0.5, Sources: []string{retrieval.SourceVector}},
		{ID: "shared", Score: 0.3, Sources: []string{retrieval.SourceBM25}},
		{ID: "only-bm25", Score: 0.2, Sources: []string{retrieval.SourceBM25}},
	}
	out := retrieval.Dedup(input)
	if len(out) != 3 {
		t.Fatalf("expected 3 unique ids; got %d (%+v)", len(out), out)
	}
	// The vector entry was higher-scored, so it must win.
	var shared *retrieval.Match
	for _, m := range out {
		if m.ID == "shared" {
			shared = m
			break
		}
	}
	if shared == nil {
		t.Fatalf("shared id missing from output")
	}
	if shared.Score != 0.8 {
		t.Fatalf("expected higher-scored entry (0.8); got %v", shared.Score)
	}
	gotSources := map[string]bool{}
	for _, s := range shared.Sources {
		gotSources[s] = true
	}
	if !gotSources[retrieval.SourceVector] || !gotSources[retrieval.SourceBM25] {
		t.Fatalf("expected union of sources {vector,bm25}; got %v", shared.Sources)
	}
}

func TestDedup_PreservesOrderForUniqueIDs(t *testing.T) {
	t.Parallel()
	input := []*retrieval.Match{
		{ID: "a", Score: 0.9},
		{ID: "b", Score: 0.8},
		{ID: "c", Score: 0.7},
	}
	out := retrieval.Dedup(input)
	if len(out) != 3 || out[0].ID != "a" || out[1].ID != "b" || out[2].ID != "c" {
		t.Fatalf("unexpected order: %+v", out)
	}
}

func TestDedup_EmptyAndSingleton(t *testing.T) {
	t.Parallel()
	if got := retrieval.Dedup(nil); got != nil {
		t.Fatalf("nil in → nil out; got %v", got)
	}
	single := []*retrieval.Match{{ID: "a", Score: 1.0}}
	out := retrieval.Dedup(single)
	if len(out) != 1 || out[0].ID != "a" {
		t.Fatalf("singleton roundtrip: %+v", out)
	}
}
