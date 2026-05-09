package policy_test

import (
	"context"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// TestLiveResolverGORM_RoundTripFromApplySnapshot verifies that
// applying a snapshot through LiveStoreGORM and then resolving it
// through LiveResolverGORM yields the same EffectiveMode, ACL, and
// recipient policy. This is the "write — read — verify" contract
// the retrieval handler and the simulator's LiveResolver depend on.
func TestLiveResolverGORM_RoundTripFromApplySnapshot(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteLiveStore(t)
	resolver := policy.NewLiveResolverGORM(db)
	ctx := context.Background()

	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow, ComputeTier: "remote"},
			{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
		}},
		Recipient: &policy.RecipientPolicy{Rules: []policy.RecipientRule{
			{SkillID: "qa", Action: policy.ACLActionAllow},
		}},
	}
	if err := store.ApplySnapshot(ctx, db, "tenant-a", "", snap); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	got, err := resolver.Resolve(ctx, "tenant-a", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.EffectiveMode != policy.PrivacyModeRemote {
		t.Fatalf("EffectiveMode: %q (want %q)", got.EffectiveMode, policy.PrivacyModeRemote)
	}
	if got.ACL == nil || len(got.ACL.Rules) != 2 {
		t.Fatalf("ACL: %+v", got.ACL)
	}
	if got.Recipient == nil || len(got.Recipient.Rules) != 1 {
		t.Fatalf("Recipient: %+v", got.Recipient)
	}
}

// TestLiveResolverGORM_StrictestModeWins applies a permissive
// tenant-wide row and a stricter channel row, then verifies the
// resolver returns the channel mode (the stricter of the two).
func TestLiveResolverGORM_StrictestModeWins(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteLiveStore(t)
	resolver := policy.NewLiveResolverGORM(db)
	ctx := context.Background()

	if err := store.ApplySnapshot(ctx, db, "tenant-a", "", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
	}); err != nil {
		t.Fatalf("ApplySnapshot tenant: %v", err)
	}
	if err := store.ApplySnapshot(ctx, db, "tenant-a", "channel-1", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeLocalOnly,
	}); err != nil {
		t.Fatalf("ApplySnapshot channel: %v", err)
	}

	got, err := resolver.Resolve(ctx, "tenant-a", "channel-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.EffectiveMode != policy.PrivacyModeLocalOnly {
		t.Fatalf("EffectiveMode: %q (want %q)", got.EffectiveMode, policy.PrivacyModeLocalOnly)
	}
}

// TestLiveResolverGORM_NoRowsLeavesModeBlank confirms the resolver
// returns an empty EffectiveMode when neither tenant_policies nor
// channel_policies has a row, so the retrieval handler falls back
// to its DefaultPrivacyMode rather than collapsing to NoAI.
func TestLiveResolverGORM_NoRowsLeavesModeBlank(t *testing.T) {
	t.Parallel()
	_, db := newSQLiteLiveStore(t)
	resolver := policy.NewLiveResolverGORM(db)
	ctx := context.Background()

	got, err := resolver.Resolve(ctx, "tenant-empty", "channel-x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.EffectiveMode != "" {
		t.Fatalf("EffectiveMode: %q (want empty)", got.EffectiveMode)
	}
	if got.ACL != nil {
		t.Fatalf("ACL: %+v (want nil)", got.ACL)
	}
}

// TestLiveResolverGORM_ChannelInheritsTenantACL verifies that a
// channel-scoped Resolve sees both tenant-wide and channel-specific
// ACL rules so admins can layer per-channel overrides on top of the
// tenant baseline.
func TestLiveResolverGORM_ChannelInheritsTenantACL(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteLiveStore(t)
	resolver := policy.NewLiveResolverGORM(db)
	ctx := context.Background()

	if err := store.ApplySnapshot(ctx, db, "tenant-a", "", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow},
		}},
	}); err != nil {
		t.Fatalf("ApplySnapshot tenant: %v", err)
	}
	if err := store.ApplySnapshot(ctx, db, "tenant-a", "channel-1", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
		}},
	}); err != nil {
		t.Fatalf("ApplySnapshot channel: %v", err)
	}

	got, err := resolver.Resolve(ctx, "tenant-a", "channel-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ACL == nil {
		t.Fatal("ACL should be populated")
	}
	verdict := got.ACL.Evaluate(policy.ChunkAttrs{Path: "drive/secret/payroll.csv"})
	if verdict.Allowed {
		t.Fatal("channel-specific deny should override tenant-wide allow")
	}
	verdict = got.ACL.Evaluate(policy.ChunkAttrs{Path: "drive/public/intro.md"})
	if !verdict.Allowed {
		t.Fatal("tenant-wide allow should still apply")
	}
}

// TestLiveResolverGORM_RejectsMissingTenant guards against silent
// cross-tenant leaks if a caller fails to pass tenantID.
func TestLiveResolverGORM_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	_, db := newSQLiteLiveStore(t)
	resolver := policy.NewLiveResolverGORM(db)
	ctx := context.Background()
	if _, err := resolver.Resolve(ctx, "", ""); err == nil {
		t.Fatal("expected error for missing tenantID")
	}
}
