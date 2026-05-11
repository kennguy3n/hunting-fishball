// chunk_scorer.go — Round-7 Task 12.
//
// ChunkScorer evaluates a chunk along four orthogonal axes
// (length, language confidence, embedding magnitude, duplicate
// ratio) and combines them into a 0..1 quality score the Stage 4
// store worker can persist alongside the chunk.
//
// The score is purely heuristic and is meant to flag obviously
// bad chunks (empty, tiny, zero-embedding, language-uncertain,
// duplicates) so an operator dashboard can highlight low-quality
// sources. A zero score does *not* block writes — that's a
// product decision left to the caller.
package pipeline

import (
	"math"
	"strings"
	"unicode"
)

// ChunkScore holds the per-axis breakdown plus the combined
// 0..1 overall score.
type ChunkScore struct {
	Length    float64
	Language  float64
	Embedding float64
	Duplicate bool
	Overall   float64
}

// ChunkScorerInput is the input shape — keep this struct stable
// even if the implementation grows.
type ChunkScorerInput struct {
	Text      string
	Embedding []float32
	Language  string
	LangConf  float64
	Duplicate bool
}

// ChunkScorer is the pure scoring function. Stateless and
// goroutine-safe.
type ChunkScorer struct {
	MinLength int
	MaxLength int
}

// NewChunkScorer returns the default scorer (100..4000 char range).
func NewChunkScorer() *ChunkScorer {
	return &ChunkScorer{MinLength: 100, MaxLength: 4000}
}

// Score returns the per-axis breakdown plus the overall score.
func (s *ChunkScorer) Score(in ChunkScorerInput) ChunkScore {
	out := ChunkScore{
		Length:    s.scoreLength(in.Text),
		Language:  s.scoreLanguage(in),
		Embedding: s.scoreEmbedding(in.Embedding),
		Duplicate: in.Duplicate,
	}
	// Duplicate caps the overall score at 0.5.
	overall := (out.Length + out.Language + out.Embedding) / 3.0
	if out.Duplicate {
		if overall > 0.5 {
			overall = 0.5
		}
	}
	out.Overall = overall
	return out
}

func (s *ChunkScorer) scoreLength(text string) float64 {
	n := 0
	for _, r := range text {
		if !unicode.IsControl(r) {
			n++
		}
	}
	if n < s.MinLength {
		return float64(n) / float64(s.MinLength)
	}
	if n > s.MaxLength {
		// Decay linearly to 0.5 by 2x max, floor at 0.25.
		excess := float64(n-s.MaxLength) / float64(s.MaxLength)
		val := 1.0 - 0.5*excess
		if val < 0.25 {
			return 0.25
		}
		return val
	}
	return 1.0
}

func (s *ChunkScorer) scoreLanguage(in ChunkScorerInput) float64 {
	if in.LangConf <= 0 {
		// No detection ran — assume neutral confidence.
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return 0
		}
		return 0.5
	}
	if in.LangConf > 1 {
		return 1
	}
	return in.LangConf
}

func (s *ChunkScorer) scoreEmbedding(emb []float32) float64 {
	if len(emb) == 0 {
		return 0
	}
	var sum float64
	for _, v := range emb {
		sum += float64(v) * float64(v)
	}
	mag := math.Sqrt(sum)
	switch {
	case mag < 0.01:
		return 0
	case mag < 0.5:
		return mag * 2 // 0.01..0.5 → 0.02..1.0
	default:
		return 1.0
	}
}
