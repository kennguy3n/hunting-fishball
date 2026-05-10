package policy_test

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// sqliteLiveSchema mirrors migrations/004_policy.sql in SQLite-
// compatible types so the live-store contract can be exercised in
// unit tests without spinning up Postgres.
const sqliteLiveSchema = `
CREATE TABLE tenant_policies (
    tenant_id     TEXT PRIMARY KEY,
    privacy_mode  TEXT NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE channel_policies (
    tenant_id              TEXT NOT NULL,
    channel_id             TEXT NOT NULL,
    privacy_mode           TEXT NOT NULL,
    recipient_default      TEXT NOT NULL DEFAULT 'allow',
    deny_local_retrieval   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, channel_id)
);

CREATE TABLE policy_acl_rules (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    channel_id    TEXT,
    source_id     TEXT,
    namespace_id  TEXT,
    path_glob     TEXT,
    action        TEXT NOT NULL,
    compute_tier  TEXT,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE recipient_policies (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    channel_id  TEXT NOT NULL,
    skill_id    TEXT,
    action      TEXT NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE namespace_policies (
    tenant_id    TEXT NOT NULL,
    namespace_id TEXT NOT NULL,
    privacy_mode TEXT NOT NULL,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, namespace_id)
);
`

func newSQLiteLiveStore(t *testing.T) (*policy.LiveStoreGORM, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteLiveSchema).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return policy.NewLiveStoreGORM(db), db
}

func countRows(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.Table(table).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestLiveStoreGORM_ApplySnapshot_TenantWide(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteLiveStore(t)
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

	if got := countRows(t, db, "tenant_policies"); got != 1 {
		t.Fatalf("tenant_policies: %d", got)
	}
	if got := countRows(t, db, "policy_acl_rules"); got != 2 {
		t.Fatalf("policy_acl_rules: %d", got)
	}
	if got := countRows(t, db, "recipient_policies"); got != 1 {
		t.Fatalf("recipient_policies: %d", got)
	}
}

func TestLiveStoreGORM_ApplySnapshot_ChannelScoped(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteLiveStore(t)
	ctx := context.Background()

	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeLocalOnly,
		Recipient:     &policy.RecipientPolicy{DefaultAllow: false},
	}
	if err := store.ApplySnapshot(ctx, db, "tenant-a", "channel-1", snap); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if got := countRows(t, db, "channel_policies"); got != 1 {
		t.Fatalf("channel_policies: %d", got)
	}
	if got := countRows(t, db, "tenant_policies"); got != 0 {
		t.Fatalf("tenant_policies must remain empty: %d", got)
	}
}

func TestLiveStoreGORM_ApplySnapshot_ReplacesACLRules(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteLiveStore(t)
	ctx := context.Background()

	first := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "old/**", Action: policy.ACLActionAllow},
		}},
	}
	if err := store.ApplySnapshot(ctx, db, "tenant-a", "", first); err != nil {
		t.Fatalf("first ApplySnapshot: %v", err)
	}

	second := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "new/**", Action: policy.ACLActionAllow},
		}},
	}
	if err := store.ApplySnapshot(ctx, db, "tenant-a", "", second); err != nil {
		t.Fatalf("second ApplySnapshot: %v", err)
	}

	// Wipe-and-replace semantics: only the new rule remains.
	if got := countRows(t, db, "policy_acl_rules"); got != 1 {
		t.Fatalf("policy_acl_rules: %d (want 1)", got)
	}
}

func TestLiveStoreGORM_ApplySnapshot_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	store, db := newSQLiteLiveStore(t)
	ctx := context.Background()
	if err := store.ApplySnapshot(ctx, db, "", "", policy.PolicySnapshot{}); err == nil {
		t.Fatal("expected error for missing tenantID")
	}
}
