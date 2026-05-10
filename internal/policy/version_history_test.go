package policy_test

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// sqliteVersionsSchema mirrors migrations/017_policy_versions.sql
// with SQLite-compatible types so promoter+history tests can use a
// single in-memory database.
const sqliteVersionsSchema = `
CREATE TABLE policy_versions (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    channel_id  TEXT NOT NULL DEFAULT '',
    draft_id    TEXT NOT NULL,
    action      TEXT NOT NULL,
    actor_id    TEXT NOT NULL DEFAULT '',
    snapshot    TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL
);
CREATE INDEX idx_policy_versions_tenant ON policy_versions (tenant_id, id);
`

func newVersionsDB(t *testing.T) (*policy.PolicyVersionRepository, *policy.DraftRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteDraftSchema).Error; err != nil {
		t.Fatalf("apply draft schema: %v", err)
	}
	if err := db.Exec(sqliteVersionsSchema).Error; err != nil {
		t.Fatalf("apply versions schema: %v", err)
	}
	return policy.NewPolicyVersionRepository(db), policy.NewDraftRepository(db), db
}

func TestPolicyVersionRepository_InsertAndList(t *testing.T) {
	t.Parallel()
	versions, _, _ := newVersionsDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		v := &policy.PolicyVersion{
			TenantID:  "tenant-a",
			ChannelID: "channel-1",
			DraftID:   "drft-1",
			Action:    string(audit.ActionPolicyPromoted),
			ActorID:   "actor",
			Snapshot:  policy.DraftPayload{Snapshot: policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}},
		}
		if err := versions.Insert(ctx, nil, v); err != nil {
			t.Fatalf("Insert[%d]: %v", i, err)
		}
		if v.ID == "" {
			t.Fatalf("Insert[%d]: ID not minted", i)
		}
	}

	rows, err := versions.List(ctx, policy.VersionListFilter{TenantID: "tenant-a", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	// Newest first.
	if rows[0].ID <= rows[1].ID || rows[1].ID <= rows[2].ID {
		t.Fatalf("rows not in descending id order: %v", rows)
	}
}

func TestPolicyVersionRepository_TenantIsolation(t *testing.T) {
	t.Parallel()
	versions, _, _ := newVersionsDB(t)
	ctx := context.Background()

	mk := func(tenant string) *policy.PolicyVersion {
		return &policy.PolicyVersion{
			TenantID: tenant, ChannelID: "c", DraftID: "d",
			Action: string(audit.ActionPolicyPromoted), ActorID: "a",
			Snapshot: policy.DraftPayload{Snapshot: policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}},
		}
	}
	if err := versions.Insert(ctx, nil, mk("tenant-a")); err != nil {
		t.Fatal(err)
	}
	if err := versions.Insert(ctx, nil, mk("tenant-b")); err != nil {
		t.Fatal(err)
	}
	rows, err := versions.List(ctx, policy.VersionListFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TenantID != "tenant-a" {
		t.Fatalf("tenant isolation broken: %+v", rows)
	}

	// Cross-tenant Get returns ErrRecordNotFound.
	bRows, _ := versions.List(ctx, policy.VersionListFilter{TenantID: "tenant-b"})
	if len(bRows) != 1 {
		t.Fatalf("setup: expected 1 row in tenant-b, got %d", len(bRows))
	}
	_, err = versions.Get(ctx, "tenant-a", bRows[0].ID)
	if err == nil {
		t.Fatalf("Get cross-tenant should return error")
	}
}

func TestPolicyVersionRepository_CursorPagination(t *testing.T) {
	t.Parallel()
	versions, _, _ := newVersionsDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = versions.Insert(ctx, nil, &policy.PolicyVersion{
			TenantID: "tenant-a", ChannelID: "c", DraftID: "d",
			Action: string(audit.ActionPolicyPromoted), ActorID: "a",
			Snapshot: policy.DraftPayload{Snapshot: policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}},
		})
	}
	first, err := versions.List(ctx, policy.VersionListFilter{TenantID: "tenant-a", Limit: 2})
	if err != nil || len(first) != 2 {
		t.Fatalf("first page: err=%v len=%d", err, len(first))
	}
	cursor := first[len(first)-1].ID
	second, err := versions.List(ctx, policy.VersionListFilter{TenantID: "tenant-a", Limit: 2, Cursor: cursor})
	if err != nil || len(second) != 2 {
		t.Fatalf("second page: err=%v len=%d", err, len(second))
	}
	for _, r := range second {
		if r.ID >= cursor {
			t.Fatalf("cursor not enforced: %s >= %s", r.ID, cursor)
		}
	}
}

func TestPromoter_RecordsVersionRow(t *testing.T) {
	t.Parallel()
	versions, drafts, _ := newVersionsDB(t)
	live := &fakeLiveStore{}
	au := &fakeAudit{}
	p, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts: drafts, LiveStore: live, Audit: au, Versions: versions,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	ctx := context.Background()
	d := policy.NewDraft("tenant-a", "channel-1", "ken", policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote})
	if err := drafts.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1"); err != nil {
		t.Fatalf("PromoteDraft: %v", err)
	}
	rows, err := versions.List(ctx, policy.VersionListFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 version row, got %d", len(rows))
	}
	if rows[0].Action != string(audit.ActionPolicyPromoted) {
		t.Fatalf("version action: %q", rows[0].Action)
	}
	if rows[0].DraftID != d.ID {
		t.Fatalf("draft id: %q", rows[0].DraftID)
	}
}
