package retrieval

// chunk_merger_test.go — Round-19 Task 23.

import (
	"strings"
	"testing"
)

func TestChunkMerger_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	m := NewChunkMerger(ChunkMergerConfig{Enabled: false})
	in := []Match{
		{ID: "a", TenantID: "t", DocumentID: "d1", Text: strings.Repeat("x", 10)},
		{ID: "b", TenantID: "t", DocumentID: "d1", Text: strings.Repeat("x", 10)},
	}
	got := m.Merge(in)
	if len(got) != 2 {
		t.Fatalf("disabled merger must return input unchanged, got %d", len(got))
	}
}

func TestChunkMerger_MergesAdjacentSameDocShortChunks(t *testing.T) {
	t.Parallel()
	m := NewChunkMerger(ChunkMergerConfig{
		Enabled:            true,
		MinTextChars:       50,
		MaxMergedTextChars: 1000,
		MaxGroupSize:       4,
	})
	in := []Match{
		{ID: "a", TenantID: "t", DocumentID: "d1", Text: "first short body", Score: 0.5, Sources: []string{"vector"}},
		{ID: "b", TenantID: "t", DocumentID: "d1", Text: "second short body", Score: 0.7, Sources: []string{"bm25"}},
		{ID: "c", TenantID: "t", DocumentID: "d1", Text: "third short body", Score: 0.6, Sources: []string{"graph"}},
	}
	got := m.Merge(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 merged match, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "first short body") ||
		!strings.Contains(got[0].Text, "second short body") ||
		!strings.Contains(got[0].Text, "third short body") {
		t.Fatalf("merged text missing bodies: %q", got[0].Text)
	}
	if got[0].Score != 0.7 {
		t.Fatalf("merged Score should be max contributing, got %v", got[0].Score)
	}
	if sz, _ := got[0].Metadata["chunk_merger_group_size"].(int); sz != 3 {
		t.Fatalf("metadata chunk_merger_group_size = %v, want 3", got[0].Metadata["chunk_merger_group_size"])
	}
}

func TestChunkMerger_DifferentDocumentsAreNotMerged(t *testing.T) {
	t.Parallel()
	m := NewChunkMerger(ChunkMergerConfig{Enabled: true, MinTextChars: 50})
	in := []Match{
		{ID: "a", TenantID: "t", DocumentID: "d1", Text: "short body a"},
		{ID: "b", TenantID: "t", DocumentID: "d2", Text: "short body b"},
	}
	got := m.Merge(in)
	if len(got) != 2 {
		t.Fatalf("matches from different documents must not be merged, got %d", len(got))
	}
}

func TestChunkMerger_LongChunksAreNotMerged(t *testing.T) {
	t.Parallel()
	m := NewChunkMerger(ChunkMergerConfig{Enabled: true, MinTextChars: 5})
	in := []Match{
		{ID: "a", TenantID: "t", DocumentID: "d1", Text: strings.Repeat("x", 50)},
		{ID: "b", TenantID: "t", DocumentID: "d1", Text: strings.Repeat("x", 50)},
	}
	got := m.Merge(in)
	if len(got) != 2 {
		t.Fatalf("matches above MinTextChars must not be merged, got %d", len(got))
	}
}

func TestChunkMerger_MaxMergedTextCharsCaps(t *testing.T) {
	t.Parallel()
	m := NewChunkMerger(ChunkMergerConfig{
		Enabled:            true,
		MinTextChars:       100,
		MaxMergedTextChars: 30, // tight cap forces a stop after the first merge
		Separator:          "\n",
	})
	in := []Match{
		{ID: "a", TenantID: "t", DocumentID: "d1", Text: "short body a"},
		{ID: "b", TenantID: "t", DocumentID: "d1", Text: "short body b"},
		{ID: "c", TenantID: "t", DocumentID: "d1", Text: "short body c"},
	}
	got := m.Merge(in)
	if len(got) < 2 {
		t.Fatalf("max-merged-text cap should produce >=2 groups, got %d", len(got))
	}
}
