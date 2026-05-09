package retrieval

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

func TestApplyPolicySnapshot_NoSnapshotPasses(t *testing.T) {
	t.Parallel()
	in := []*Match{{ID: "a"}, {ID: "b"}}
	got, byACL, byRecip := applyPolicySnapshot(in, PolicySnapshot{}, "skill")
	if len(got) != 2 || byACL != 0 || byRecip != 0 {
		t.Fatalf("expected pass-through, got len=%d acl=%d recip=%d", len(got), byACL, byRecip)
	}
}

func TestApplyPolicySnapshot_RecipientDeniesAll(t *testing.T) {
	t.Parallel()
	in := []*Match{{ID: "a"}, {ID: "b"}}
	snap := PolicySnapshot{
		Recipient: &policy.RecipientPolicy{
			Rules: []policy.RecipientRule{
				{SkillID: "exfil", Action: policy.ACLActionDeny},
			},
			DefaultAllow: true,
		},
	}
	got, byACL, byRecip := applyPolicySnapshot(in, snap, "exfil")
	if len(got) != 0 || byRecip != 2 || byACL != 0 {
		t.Fatalf("recipient deny must drop all: len=%d acl=%d recip=%d", len(got), byACL, byRecip)
	}
}

func TestApplyPolicySnapshot_ACLBlocksByPath(t *testing.T) {
	t.Parallel()
	in := []*Match{
		{ID: "ok", SourceID: "s1", Metadata: map[string]any{"path": "docs/intro.md"}},
		{ID: "bad", SourceID: "s1", Metadata: map[string]any{"path": "secrets/keys.txt"}},
	}
	snap := PolicySnapshot{
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{PathGlob: "secrets/*", Action: policy.ACLActionDeny},
			},
		},
	}
	got, byACL, byRecip := applyPolicySnapshot(in, snap, "summarizer")
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("ACL did not drop secrets/keys.txt: %+v", got)
	}
	if byACL != 1 || byRecip != 0 {
		t.Fatalf("counters: acl=%d recip=%d", byACL, byRecip)
	}
}

func TestApplyPolicySnapshot_ACLByNamespace(t *testing.T) {
	t.Parallel()
	in := []*Match{
		{ID: "a", SourceID: "s1", Metadata: map[string]any{"namespace_id": "drive-1"}},
		{ID: "b", SourceID: "s1", Metadata: map[string]any{"namespace_id": "drive-2"}},
	}
	snap := PolicySnapshot{
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{NamespaceID: "drive-2", Action: policy.ACLActionDeny},
			},
		},
	}
	got, byACL, _ := applyPolicySnapshot(in, snap, "skill")
	if len(got) != 1 || got[0].ID != "a" || byACL != 1 {
		t.Fatalf("namespace-scoped deny broken: got=%+v acl=%d", got, byACL)
	}
}

func TestApplyPolicySnapshot_NilRecipientCatchesAll(t *testing.T) {
	t.Parallel()
	in := []*Match{{ID: "a"}}
	snap := PolicySnapshot{}
	got, _, _ := applyPolicySnapshot(in, snap, "")
	if len(got) != 1 {
		t.Fatalf("nil snapshot must pass-through: %+v", got)
	}
}

func TestFilterCachedBySnapshot_NoSnapshotPasses(t *testing.T) {
	t.Parallel()
	in := &storage.CachedResult{Hits: []storage.CachedHit{{ID: "a"}, {ID: "b"}}}
	out, byACL, byRecip := filterCachedBySnapshot(in, PolicySnapshot{}, "skill")
	if out != in {
		t.Fatalf("no-op snapshot should return the input pointer unchanged")
	}
	if byACL != 0 || byRecip != 0 {
		t.Fatalf("counters: acl=%d recip=%d", byACL, byRecip)
	}
}

func TestFilterCachedBySnapshot_ACLDropsByPath(t *testing.T) {
	t.Parallel()
	in := &storage.CachedResult{Hits: []storage.CachedHit{
		{ID: "ok", SourceID: "s1", Metadata: map[string]any{"path": "docs/intro.md"}},
		{ID: "bad", SourceID: "s1", Metadata: map[string]any{"path": "secrets/keys.txt"}},
	}}
	snap := PolicySnapshot{
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "secrets/*", Action: policy.ACLActionDeny},
		}},
	}
	out, byACL, byRecip := filterCachedBySnapshot(in, snap, "summarizer")
	if len(out.Hits) != 1 || out.Hits[0].ID != "ok" {
		t.Fatalf("ACL did not drop secrets/keys.txt from cache: %+v", out.Hits)
	}
	if byACL != 1 || byRecip != 0 {
		t.Fatalf("counters: acl=%d recip=%d", byACL, byRecip)
	}
	if len(out.ChunkIDs) != 1 || out.ChunkIDs[0] != "ok" {
		t.Fatalf("chunk_ids should track filtered set: %v", out.ChunkIDs)
	}
}

func TestFilterCachedBySnapshot_RecipientDeniesAll(t *testing.T) {
	t.Parallel()
	in := &storage.CachedResult{Hits: []storage.CachedHit{{ID: "a"}, {ID: "b"}}}
	snap := PolicySnapshot{
		Recipient: &policy.RecipientPolicy{
			Rules: []policy.RecipientRule{
				{SkillID: "exfil", Action: policy.ACLActionDeny},
			},
			DefaultAllow: true,
		},
	}
	out, byACL, byRecip := filterCachedBySnapshot(in, snap, "exfil")
	if len(out.Hits) != 0 {
		t.Fatalf("recipient deny must drop every cached hit: %+v", out.Hits)
	}
	if byACL != 0 || byRecip != 2 {
		t.Fatalf("counters: acl=%d recip=%d", byACL, byRecip)
	}
}

func TestFilterCachedBySnapshot_NilCacheReturnsNil(t *testing.T) {
	t.Parallel()
	out, byACL, byRecip := filterCachedBySnapshot(nil, PolicySnapshot{}, "")
	if out != nil || byACL != 0 || byRecip != 0 {
		t.Fatalf("nil cache must round-trip as nil: %+v acl=%d recip=%d", out, byACL, byRecip)
	}
}
