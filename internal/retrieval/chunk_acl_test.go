package retrieval

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestApplyPolicySnapshot_ChunkACLDeny(t *testing.T) {
	snap := PolicySnapshot{
		ChunkACL: policy.NewChunkACL([]policy.ChunkACLTag{
			{ChunkID: "ch-2", Decision: policy.ChunkACLDecisionDeny},
		}),
	}
	matches := []*Match{
		{ID: "ch-1", SourceID: "s"},
		{ID: "ch-2", SourceID: "s"},
		{ID: "ch-3", SourceID: "s"},
	}
	kept, blocked, recip := applyPolicySnapshot(matches, snap, "")
	if recip != 0 {
		t.Fatalf("unexpected recip block: %d", recip)
	}
	if blocked != 1 {
		t.Fatalf("expected 1 chunk denied; got %d", blocked)
	}
	if len(kept) != 2 {
		t.Fatalf("expected 2 chunks kept; got %d", len(kept))
	}
	for _, m := range kept {
		if m.ID == "ch-2" {
			t.Fatalf("ch-2 must be filtered out")
		}
	}
}

func TestApplyPolicySnapshot_ChunkACLInheritsFromSource(t *testing.T) {
	// All chunks belonging to source `s-bad` are denied via the
	// `source.<id>` synthetic tag.
	snap := PolicySnapshot{
		ChunkACL: policy.NewChunkACL([]policy.ChunkACLTag{
			{TagPrefix: "source.s-bad", Decision: policy.ChunkACLDecisionDeny},
		}),
	}
	matches := []*Match{
		{ID: "ch-1", SourceID: "s-good"},
		{ID: "ch-2", SourceID: "s-bad"},
	}
	kept, blocked, _ := applyPolicySnapshot(matches, snap, "")
	if blocked != 1 || len(kept) != 1 || kept[0].ID != "ch-1" {
		t.Fatalf("inheritance broken: blocked=%d kept=%v", blocked, ids(kept))
	}
}

func TestApplyPolicySnapshot_ChunkACLAllowedWhenNoMatch(t *testing.T) {
	snap := PolicySnapshot{
		ChunkACL: policy.NewChunkACL([]policy.ChunkACLTag{
			{ChunkID: "different-chunk", Decision: policy.ChunkACLDecisionDeny},
		}),
	}
	matches := []*Match{{ID: "ch-1", SourceID: "s"}}
	kept, blocked, _ := applyPolicySnapshot(matches, snap, "")
	if blocked != 0 || len(kept) != 1 {
		t.Fatalf("default-allow broken: blocked=%d kept=%d", blocked, len(kept))
	}
}

func TestChunkTags_FromMetadata(t *testing.T) {
	m := &Match{ID: "x", SourceID: "src", Metadata: map[string]any{
		"chunk_tags": []any{"pii.ssn", "topic.legal"},
	}}
	tags := chunkTags(m)
	want := map[string]bool{"source.src": true, "pii.ssn": true, "topic.legal": true}
	if len(tags) != 3 {
		t.Fatalf("got %v, want 3 tags", tags)
	}
	for _, tag := range tags {
		if !want[tag] {
			t.Fatalf("unexpected tag %q in %v", tag, tags)
		}
	}
}
