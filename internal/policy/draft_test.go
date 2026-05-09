package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// sqliteDraftSchema is a SQLite-compatible mirror of
// migrations/005_policy_drafts.sql. Production uses Postgres types
// (jsonb, char(26), tsz); SQLite stores them as TEXT/DATETIME, which
// is good enough for repository-level behaviour.
const sqliteDraftSchema = `
CREATE TABLE policy_drafts (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    channel_id    TEXT,
    payload       TEXT NOT NULL DEFAULT '{}',
    status        TEXT NOT NULL DEFAULT 'draft',
    created_by    TEXT,
    created_at    DATETIME NOT NULL,
    promoted_at   DATETIME,
    promoted_by   TEXT,
    reject_reason TEXT
);
CREATE INDEX idx_policy_drafts_tenant ON policy_drafts (tenant_id, status);
`

func newSQLiteDraftRepo(t *testing.T) (*policy.DraftRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteDraftSchema).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return policy.NewDraftRepository(db), db
}

func TestDraftRepository_CreateAndGet_RoundTripsSnapshot(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteDraftRepo(t)
	ctx := context.Background()

	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeHybrid,
		ACL: &policy.AllowDenyList{
			TenantID: "tenant-a",
			Rules: []policy.ACLRule{
				{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
			},
		},
		Recipient: &policy.RecipientPolicy{
			TenantID: "tenant-a",
			Rules:    []policy.RecipientRule{{SkillID: "qa", Action: policy.ACLActionAllow}},
		},
	}
	d := policy.NewDraft("tenant-a", "channel-1", "ken", snap)
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, "tenant-a", d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != policy.DraftStatusDraft {
		t.Fatalf("status: %q", got.Status)
	}
	if got.Payload.Snapshot.EffectiveMode != policy.PrivacyModeHybrid {
		t.Fatalf("EffectiveMode round-trip: %q", got.Payload.Snapshot.EffectiveMode)
	}
	if got.Payload.Snapshot.ACL == nil || len(got.Payload.Snapshot.ACL.Rules) != 1 {
		t.Fatalf("ACL round-trip: %+v", got.Payload.Snapshot.ACL)
	}
	if got.Payload.Snapshot.Recipient == nil || len(got.Payload.Snapshot.Recipient.Rules) != 1 {
		t.Fatalf("Recipient round-trip: %+v", got.Payload.Snapshot.Recipient)
	}
}

func TestDraftRepository_Get_TenantIsolation(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteDraftRepo(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.Get(ctx, "tenant-b", d.ID); !errors.Is(err, policy.ErrDraftNotFound) {
		t.Fatalf("cross-tenant Get must return ErrDraftNotFound, got %v", err)
	}
}

func TestDraftRepository_List_TenantOnly(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteDraftRepo(t)
	ctx := context.Background()

	for _, tenant := range []string{"tenant-a", "tenant-b", "tenant-a", "tenant-a"} {
		d := policy.NewDraft(tenant, "", "ken", policy.PolicySnapshot{})
		if err := repo.Create(ctx, d); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	got, err := repo.List(ctx, policy.DraftListFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 drafts for tenant-a, got %d", len(got))
	}
	for _, d := range got {
		if d.TenantID != "tenant-a" {
			t.Fatalf("tenant leak: %+v", d)
		}
	}
}

func TestDraftRepository_List_StatusFilter(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteDraftRepo(t)
	ctx := context.Background()

	d1 := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	d2 := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	for _, d := range []*policy.Draft{d1, d2} {
		if err := repo.Create(ctx, d); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if _, err := repo.MarkPromoted(ctx, db, "tenant-a", d1.ID, "actor-1"); err != nil {
		t.Fatalf("MarkPromoted: %v", err)
	}

	drafts, err := repo.List(ctx, policy.DraftListFilter{TenantID: "tenant-a", Status: policy.DraftStatusDraft})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(drafts) != 1 || drafts[0].ID != d2.ID {
		t.Fatalf("status filter: got %+v", drafts)
	}
}

func TestDraftRepository_MarkPromoted_FlipsStatus(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteDraftRepo(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.MarkPromoted(ctx, db, "tenant-a", d.ID, "actor-1")
	if err != nil {
		t.Fatalf("MarkPromoted: %v", err)
	}
	if got.Status != policy.DraftStatusPromoted {
		t.Fatalf("status: %q", got.Status)
	}
	if got.PromotedAt == nil {
		t.Fatal("PromotedAt should be set")
	}
	if got.PromotedBy != "actor-1" {
		t.Fatalf("PromotedBy: %q", got.PromotedBy)
	}
}

func TestDraftRepository_MarkPromoted_RejectsTerminal(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteDraftRepo(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.MarkPromoted(ctx, db, "tenant-a", d.ID, "actor-1"); err != nil {
		t.Fatalf("first MarkPromoted: %v", err)
	}
	_, err := repo.MarkPromoted(ctx, db, "tenant-a", d.ID, "actor-1")
	if !errors.Is(err, policy.ErrDraftTerminal) {
		t.Fatalf("expected ErrDraftTerminal, got %v", err)
	}
}

func TestDraftRepository_MarkRejected_RecordsReason(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteDraftRepo(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.MarkRejected(ctx, db, "tenant-a", d.ID, "actor-1", "conflicts unresolved")
	if err != nil {
		t.Fatalf("MarkRejected: %v", err)
	}
	if got.Status != policy.DraftStatusRejected {
		t.Fatalf("status: %q", got.Status)
	}
	if got.RejectReason != "conflicts unresolved" {
		t.Fatalf("RejectReason: %q", got.RejectReason)
	}
}

func TestDraftRepository_MarkRejected_TenantIsolation(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteDraftRepo(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.MarkRejected(ctx, db, "tenant-b", d.ID, "actor-1", ""); !errors.Is(err, policy.ErrDraftNotFound) {
		t.Fatalf("cross-tenant MarkRejected must return ErrDraftNotFound, got %v", err)
	}
}

func TestDraftRepository_Create_RejectsInvalid(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteDraftRepo(t)
	ctx := context.Background()

	d := &policy.Draft{Status: policy.DraftStatusDraft} // missing ID + tenant
	if err := repo.Create(ctx, d); err == nil {
		t.Fatal("expected validation error")
	}
}
