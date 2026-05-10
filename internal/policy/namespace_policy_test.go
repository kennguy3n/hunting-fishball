package policy_test

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func newSQLiteFullPolicyStore(t *testing.T) (*policy.LiveStoreGORM, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteLiveSchema).Error; err != nil {
		t.Fatalf("schema live: %v", err)
	}
	return policy.NewLiveStoreGORM(db), db
}

func TestEffectiveModeForNamespace_StrictestWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		tenant     policy.PrivacyMode
		channel    policy.PrivacyMode
		namespaces map[string]policy.PrivacyMode
		nsID       string
		want       policy.PrivacyMode
	}{
		{
			name:    "no namespace falls back to tenant/channel",
			tenant:  policy.PrivacyModeRemote,
			channel: policy.PrivacyModeRemote,
			nsID:    "engineering",
			want:    policy.PrivacyModeRemote,
		},
		{
			name:       "namespace tightens remote to local-only",
			tenant:     policy.PrivacyModeRemote,
			channel:    policy.PrivacyModeRemote,
			namespaces: map[string]policy.PrivacyMode{"hr": policy.PrivacyModeLocalOnly},
			nsID:       "hr",
			want:       policy.PrivacyModeLocalOnly,
		},
		{
			name:       "namespace cannot widen channel",
			tenant:     policy.PrivacyModeRemote,
			channel:    policy.PrivacyModeLocalOnly,
			namespaces: map[string]policy.PrivacyMode{"hr": policy.PrivacyModeRemote},
			nsID:       "hr",
			want:       policy.PrivacyModeLocalOnly,
		},
		{
			name:       "empty namespace id returns tenant/channel",
			tenant:     policy.PrivacyModeRemote,
			channel:    policy.PrivacyModeHybrid,
			namespaces: map[string]policy.PrivacyMode{"hr": policy.PrivacyModeNoAI},
			nsID:       "",
			want:       policy.PrivacyModeHybrid,
		},
		{
			name:       "unknown namespace falls back",
			tenant:     policy.PrivacyModeRemote,
			channel:    policy.PrivacyModeRemote,
			namespaces: map[string]policy.PrivacyMode{"hr": policy.PrivacyModeNoAI},
			nsID:       "engineering",
			want:       policy.PrivacyModeRemote,
		},
		{
			name:       "namespace pins to no-ai",
			tenant:     policy.PrivacyModeRemote,
			channel:    policy.PrivacyModeRemote,
			namespaces: map[string]policy.PrivacyMode{"finance": policy.PrivacyModeNoAI},
			nsID:       "finance",
			want:       policy.PrivacyModeNoAI,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := policy.EffectiveModeForNamespace(tc.tenant, tc.channel, tc.namespaces, tc.nsID)
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestNamespacePolicies_Lifecycle(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteFullPolicyStore(t)
	ctx := context.Background()

	if err := store.UpsertNamespacePolicy(ctx, "tenant-a", "hr", policy.PrivacyModeLocalOnly); err != nil {
		t.Fatalf("Upsert hr: %v", err)
	}
	if err := store.UpsertNamespacePolicy(ctx, "tenant-a", "engineering", policy.PrivacyModeHybrid); err != nil {
		t.Fatalf("Upsert engineering: %v", err)
	}

	got, err := store.ListNamespacePolicies(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got["hr"] != policy.PrivacyModeLocalOnly {
		t.Fatalf("hr: %q", got["hr"])
	}
	if got["engineering"] != policy.PrivacyModeHybrid {
		t.Fatalf("engineering: %q", got["engineering"])
	}

	// Update is in-place; subsequent reads see the new mode.
	if err := store.UpsertNamespacePolicy(ctx, "tenant-a", "engineering", policy.PrivacyModeNoAI); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	got, _ = store.ListNamespacePolicies(ctx, "tenant-a")
	if got["engineering"] != policy.PrivacyModeNoAI {
		t.Fatalf("after update: %q", got["engineering"])
	}

	// Delete removes the row.
	if err := store.DeleteNamespacePolicy(ctx, "tenant-a", "engineering"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ = store.ListNamespacePolicies(ctx, "tenant-a")
	if _, ok := got["engineering"]; ok {
		t.Fatalf("engineering should be gone, got %+v", got)
	}
	if got["hr"] != policy.PrivacyModeLocalOnly {
		t.Fatalf("hr still present check: %q", got["hr"])
	}
}

func TestNamespacePolicies_TenantIsolation(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteFullPolicyStore(t)
	ctx := context.Background()

	if err := store.UpsertNamespacePolicy(ctx, "tenant-a", "hr", policy.PrivacyModeLocalOnly); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.ListNamespacePolicies(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("List tenant-b: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-tenant leak: %+v", got)
	}
}

func TestNamespacePolicies_RejectsInvalidMode(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteFullPolicyStore(t)
	ctx := context.Background()

	err := store.UpsertNamespacePolicy(ctx, "tenant-a", "hr", policy.PrivacyMode("bogus"))
	if err == nil {
		t.Fatalf("expected error on invalid mode")
	}
}

func TestNamespacePolicies_RequiresIDs(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteFullPolicyStore(t)
	ctx := context.Background()

	if err := store.UpsertNamespacePolicy(ctx, "", "hr", policy.PrivacyModeLocalOnly); err == nil {
		t.Fatalf("expected error on empty tenant")
	}
	if err := store.UpsertNamespacePolicy(ctx, "tenant-a", "", policy.PrivacyModeLocalOnly); err == nil {
		t.Fatalf("expected error on empty namespace")
	}
	if err := store.DeleteNamespacePolicy(ctx, "", ""); err == nil {
		t.Fatalf("expected error on empty IDs")
	}
}

func TestLiveResolver_NamespacePoliciesLoaded(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteFullPolicyStore(t)
	ctx := context.Background()

	if err := store.UpsertNamespacePolicy(ctx, "tenant-a", "hr", policy.PrivacyModeLocalOnly); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	resolver := policy.NewLiveResolverGORM(db)
	snap, err := resolver.Resolve(ctx, "tenant-a", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := snap.NamespacePolicies["hr"]; got != policy.PrivacyModeLocalOnly {
		t.Fatalf("snap.NamespacePolicies[hr]: %q", got)
	}

	// Resolve still returns nil snapshot map when no rows exist.
	snap, err = resolver.Resolve(ctx, "tenant-empty", "")
	if err != nil {
		t.Fatalf("Resolve empty: %v", err)
	}
	if len(snap.NamespacePolicies) != 0 {
		t.Fatalf("empty tenant has rows: %+v", snap.NamespacePolicies)
	}
}

func TestSnapshotClone_DeepCopiesNamespaces(t *testing.T) {
	t.Parallel()
	orig := policy.PolicySnapshot{
		NamespacePolicies: map[string]policy.PrivacyMode{
			"hr": policy.PrivacyModeLocalOnly,
		},
	}
	clone := orig.Clone()
	clone.NamespacePolicies["hr"] = policy.PrivacyModeNoAI
	clone.NamespacePolicies["new"] = policy.PrivacyModeRemote
	if orig.NamespacePolicies["hr"] != policy.PrivacyModeLocalOnly {
		t.Fatalf("clone mutated original: %q", orig.NamespacePolicies["hr"])
	}
	if _, ok := orig.NamespacePolicies["new"]; ok {
		t.Fatalf("clone leaked new key into original")
	}
}
