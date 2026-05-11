package pipeline_test

import (
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestChunkScorer_Length(t *testing.T) {
	s := pipeline.NewChunkScorer()
	cases := []struct {
		name string
		text string
		want func(float64) bool
	}{
		{"tiny", "hi", func(v float64) bool { return v < 0.1 }},
		{"medium", strings.Repeat("a", 500), func(v float64) bool { return v == 1.0 }},
		{"too long", strings.Repeat("a", 9000), func(v float64) bool { return v <= 0.5 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := s.Score(pipeline.ChunkScorerInput{Text: c.text, Embedding: []float32{1, 1, 1}, LangConf: 1})
			if !c.want(out.Length) {
				t.Fatalf("length score for %s = %v", c.name, out.Length)
			}
		})
	}
}

func TestChunkScorer_Embedding(t *testing.T) {
	s := pipeline.NewChunkScorer()
	out := s.Score(pipeline.ChunkScorerInput{Text: strings.Repeat("a", 200), Embedding: []float32{}, LangConf: 1})
	if out.Embedding != 0 {
		t.Fatalf("expected 0 for empty embedding; got %v", out.Embedding)
	}
	out = s.Score(pipeline.ChunkScorerInput{Text: strings.Repeat("a", 200), Embedding: []float32{0.0001}, LangConf: 1})
	if out.Embedding != 0 {
		t.Fatalf("expected 0 for near-zero magnitude; got %v", out.Embedding)
	}
	out = s.Score(pipeline.ChunkScorerInput{Text: strings.Repeat("a", 200), Embedding: []float32{1, 1, 1}, LangConf: 1})
	if out.Embedding < 0.99 {
		t.Fatalf("expected ~1 for high magnitude; got %v", out.Embedding)
	}
}

func TestChunkScorer_DuplicateCapsOverall(t *testing.T) {
	s := pipeline.NewChunkScorer()
	in := pipeline.ChunkScorerInput{Text: strings.Repeat("a", 200), Embedding: []float32{1, 1, 1}, LangConf: 1, Duplicate: true}
	out := s.Score(in)
	if out.Overall > 0.5 {
		t.Fatalf("expected duplicate-capped overall <=0.5; got %v", out.Overall)
	}
}

func TestChunkScorer_Language(t *testing.T) {
	s := pipeline.NewChunkScorer()
	out := s.Score(pipeline.ChunkScorerInput{Text: strings.Repeat("a", 200), Embedding: []float32{1, 1, 1}, LangConf: 0.7})
	if out.Language != 0.7 {
		t.Fatalf("expected language=0.7; got %v", out.Language)
	}
	out = s.Score(pipeline.ChunkScorerInput{Text: "", Embedding: []float32{1, 1, 1}, LangConf: 0})
	if out.Language != 0 {
		t.Fatalf("expected 0 for empty text; got %v", out.Language)
	}
}
