package shard_test

import (
	"context"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

type fakeChunkSource struct {
	chunks []shard.ChunkScope
}

func (f *fakeChunkSource) ListChunks(_ context.Context, tenantID string) ([]shard.ChunkScope, error) {
	if tenantID == "" {
		return nil, shard.ErrMissingTenantScope
	}
	return f.chunks, nil
}

type stubResolver struct {
	snap policy.PolicySnapshot
}

func (s stubResolver) Resolve(_ context.Context, _, _ string) (policy.PolicySnapshot, error) {
	return s.snap, nil
}

func TestGenerator_GeneratesShardWithPolicyFilter(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	src := &fakeChunkSource{
		chunks: []shard.ChunkScope{
			{ChunkID: "c1", SourceID: "s1", URI: "drive/public/x.md", PrivacyLabel: "internal"},
			{ChunkID: "c2", SourceID: "s1", URI: "drive/secret/y.md", PrivacyLabel: "internal"},
			{ChunkID: "c3", SourceID: "s1", URI: "drive/public/z.md", PrivacyLabel: "no_ai"},
		},
	}

	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeHybrid,
		ACL: &policy.AllowDenyList{
			TenantID: "tenant-a",
			Rules: []policy.ACLRule{
				{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
			},
		},
	}

	gen, err := shard.NewGenerator(shard.GeneratorConfig{
		Repo:   repo,
		Chunks: src,
		Policy: stubResolver{snap: snap},
	})
	if err != nil {
		t.Fatalf("new generator: %v", err)
	}

	res, err := gen.Generate(context.Background(), shard.GenerateRequest{
		TenantID:    "tenant-a",
		UserID:      "user-1",
		ChannelID:   "channel-1",
		PrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.Manifest.Status != shard.ShardStatusReady {
		t.Fatalf("status: %q", res.Manifest.Status)
	}
	// c2 dropped by ACL deny; c3 dropped by privacy floor (no_ai
	// stricter than internal).
	if len(res.ChunkIDs) != 1 || res.ChunkIDs[0] != "c1" {
		t.Fatalf("unexpected chunk set: %v", res.ChunkIDs)
	}
	if res.Manifest.ShardVersion != 1 {
		t.Fatalf("first version: %d", res.Manifest.ShardVersion)
	}
	if res.Manifest.ChunksCount != 1 {
		t.Fatalf("chunks count: %d", res.Manifest.ChunksCount)
	}
}

func TestGenerator_BumpsVersionAndSupersedes(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	src := &fakeChunkSource{
		chunks: []shard.ChunkScope{
			{ChunkID: "c1", PrivacyLabel: "internal"},
		},
	}
	gen, err := shard.NewGenerator(shard.GeneratorConfig{
		Repo:   repo,
		Chunks: src,
		Policy: stubResolver{},
	})
	if err != nil {
		t.Fatalf("new gen: %v", err)
	}

	req := shard.GenerateRequest{
		TenantID:    "tenant-a",
		UserID:      "user-1",
		ChannelID:   "channel-1",
		PrivacyMode: "internal",
	}
	r1, err := gen.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("gen 1: %v", err)
	}
	r2, err := gen.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("gen 2: %v", err)
	}
	if r1.Manifest.ShardVersion != 1 || r2.Manifest.ShardVersion != 2 {
		t.Fatalf("versions: %d %d", r1.Manifest.ShardVersion, r2.Manifest.ShardVersion)
	}

	// r1 should now be superseded.
	scope := shard.ScopeFilter{
		TenantID:    "tenant-a",
		UserID:      "user-1",
		ChannelID:   "channel-1",
		PrivacyMode: "internal",
	}
	prev, err := repo.GetByVersion(context.Background(), scope, 1)
	if err != nil {
		t.Fatalf("get prev: %v", err)
	}
	if prev.Status != shard.ShardStatusSuperseded {
		t.Fatalf("prev status: %q", prev.Status)
	}
}

func TestGenerator_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	gen, err := shard.NewGenerator(shard.GeneratorConfig{
		Repo:   newTestRepo(t),
		Chunks: &fakeChunkSource{},
		Policy: stubResolver{},
	})
	if err != nil {
		t.Fatalf("new gen: %v", err)
	}
	if _, err := gen.Generate(context.Background(), shard.GenerateRequest{}); err == nil {
		t.Fatal("expected missing-tenant error")
	}
}

// TestGenerator_FiltersByChunkACL — Round-11 Task 8.
//
// Confirms that after the source-level ACL approves a chunk, the
// per-chunk ACL on the policy snapshot still strips chunks whose
// chunk_id is explicitly denied (or whose tags match a deny rule).
func TestGenerator_FiltersByChunkACL(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	src := &fakeChunkSource{
		chunks: []shard.ChunkScope{
			{ChunkID: "c-keep", SourceID: "s1", URI: "drive/x.md", PrivacyLabel: "internal", Tags: []string{"general"}},
			{ChunkID: "c-deny-by-id", SourceID: "s1", URI: "drive/y.md", PrivacyLabel: "internal", Tags: []string{"general"}},
			{ChunkID: "c-deny-by-tag", SourceID: "s1", URI: "drive/z.md", PrivacyLabel: "internal", Tags: []string{"pii-ssn"}},
		},
	}

	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeHybrid,
		ChunkACL: policy.NewChunkACL([]policy.ChunkACLTag{
			{ChunkID: "c-deny-by-id", Decision: policy.ChunkACLDecisionDeny},
			{TagPrefix: "pii-", Decision: policy.ChunkACLDecisionDeny},
		}),
	}

	gen, err := shard.NewGenerator(shard.GeneratorConfig{
		Repo:   repo,
		Chunks: src,
		Policy: stubResolver{snap: snap},
	})
	if err != nil {
		t.Fatalf("new generator: %v", err)
	}

	res, err := gen.Generate(context.Background(), shard.GenerateRequest{
		TenantID:    "tenant-acl",
		UserID:      "user-1",
		ChannelID:   "channel-1",
		PrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(res.ChunkIDs) != 1 || res.ChunkIDs[0] != "c-keep" {
		t.Fatalf("unexpected chunk set after ChunkACL: %v", res.ChunkIDs)
	}
}
