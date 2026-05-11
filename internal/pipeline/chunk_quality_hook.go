package pipeline

// chunk_quality_hook.go — Round-10 Task 4.
//
// Coordinator-side Stage 4 pre-write hook that runs the existing
// ChunkScorer (chunk_scorer.go) over each (block, embedding) pair
// and writes a quality report to the admin store. Best-effort:
// scoring or write errors are observed via the per-call return
// but never block the chunk's Stage 4 write — losing a score must
// not lose the chunk.

import (
	"context"
)

// ChunkQualityReport is the package-neutral shape the coordinator
// hands the recorder. The admin GORM store converts each field to
// the corresponding column on retrieval_chunks_quality.
type ChunkQualityReport struct {
	TenantID     string
	SourceID     string
	DocumentID   string
	ChunkID      string
	QualityScore float64
	LengthScore  float64
	LangScore    float64
	EmbedScore   float64
	Duplicate    bool
}

// ChunkQualityRecorder persists chunk quality reports keyed by
// (tenant_id, chunk_id). admin.ChunkQualityStoreGORM satisfies it
// (and the in-memory test fake).
type ChunkQualityRecorder interface {
	Record(ctx context.Context, report ChunkQualityReport) error
}

// scoreAndRecordBlocks runs the ChunkScorer over each (block,
// embedding) pair and persists the result. Called from Stage 4
// after embeddings are attached and before the Store.Store call.
//
// Returns the number of quality reports successfully written;
// errors are swallowed (and would be logged in production wiring).
func (c *Coordinator) scoreAndRecordBlocks(ctx context.Context, it stagedItem) int {
	if c.cfg.ChunkScorer == nil || c.cfg.ChunkQuality == nil {
		return 0
	}
	if it.doc == nil {
		return 0
	}
	written := 0
	for i, b := range it.blocks {
		var emb []float32
		if i < len(it.embeddings) {
			emb = it.embeddings[i]
		}
		score := c.cfg.ChunkScorer.Score(ChunkScorerInput{
			Text:      b.Text,
			Embedding: emb,
		})
		report := ChunkQualityReport{
			TenantID:     it.doc.TenantID,
			SourceID:     it.doc.SourceID,
			DocumentID:   it.doc.DocumentID,
			ChunkID:      b.BlockID,
			QualityScore: score.Overall,
			LengthScore:  score.Length,
			LangScore:    score.Language,
			EmbedScore:   score.Embedding,
			Duplicate:    score.Duplicate,
		}
		if err := c.cfg.ChunkQuality.Record(ctx, report); err == nil {
			written++
		}
	}
	return written
}
