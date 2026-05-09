package policy_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// captureLogger records every SQL statement gorm executes so a test
// can assert on the on-the-wire query (e.g. that a SELECT carries
// `FOR UPDATE`). Mirrors the LogMode no-op pattern from gorm's
// internal logger so it integrates without changing log levels.
type captureLogger struct {
	mu      sync.Mutex
	queries []string
}

func (c *captureLogger) LogMode(_ logger.LogLevel) logger.Interface  { return c }
func (c *captureLogger) Info(_ context.Context, _ string, _ ...any)  {}
func (c *captureLogger) Warn(_ context.Context, _ string, _ ...any)  {}
func (c *captureLogger) Error(_ context.Context, _ string, _ ...any) {}
func (c *captureLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, sql)
}

func (c *captureLogger) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.queries))
	copy(out, c.queries)
	return out
}

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

// TestDraftRepository_MarkPromoted_UsesSelectForUpdate is the
// regression test for the TOCTOU race that Devin Review caught on
// PR #6. The transition() helper reads the row, checks
// status='draft', then writes the promoted state. Without
// `SELECT … FOR UPDATE`, two concurrent Postgres transactions at
// the default READ COMMITTED isolation could both pass the
// status check, both call apply()+Save(), and both emit a
// policy.promoted audit row — so the live snapshot would be
// re-applied a second time and the audit feed would carry a
// duplicate entry. The fix attaches clause.Locking{Strength:
// "UPDATE"} to the SELECT so the second transaction blocks until
// the first commits, then sees status='promoted' and bails with
// ErrDraftTerminal.
//
// We exercise the real MarkPromoted code path against an
// in-memory SQLite db, but explicitly delete the SQLite dialect's
// no-op override of the FOR clause builder so gorm renders the
// default "FOR UPDATE" SQL we asked for via clause.Locking. We
// then capture every emitted statement via a gorm.Logger and
// assert the SELECT against policy_drafts carries FOR UPDATE.
// Without this dialect tweak SQLite would silently strip the
// locking clause and the test could not catch a regression in
// transition().
func TestDraftRepository_MarkPromoted_UsesSelectForUpdate(t *testing.T) {
	t.Parallel()

	cap := &captureLogger{}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: cap})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteDraftSchema).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// Drop the SQLite dialect's no-op FOR clause builder so the
	// default clause.Locking renderer runs and emits "FOR UPDATE"
	// — matching what Postgres does in production.
	delete(db.ClauseBuilders, "FOR")
	repo := policy.NewDraftRepository(db)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// SQLite parses "FOR UPDATE" as a syntax error, but gorm
	// builds + logs the SQL via the captureLogger before
	// dispatching the query, so the rendered statement is in the
	// captureLogger by the time MarkPromoted returns. The error
	// is expected and intentionally ignored.
	_, _ = repo.MarkPromoted(ctx, db, "tenant-a", d.ID, "actor-1")

	// Walk every captured SQL statement and confirm the SELECT
	// against policy_drafts carries the FOR UPDATE clause. Match
	// case-insensitively so a future driver-style flip (e.g.
	// gorm rendering "for update") doesn't silently regress us.
	var sawLockedSelect bool
	for _, q := range cap.snapshot() {
		upper := strings.ToUpper(q)
		if strings.Contains(upper, "SELECT") &&
			strings.Contains(upper, "POLICY_DRAFTS") &&
			strings.Contains(upper, "FOR UPDATE") {
			sawLockedSelect = true
			break
		}
	}
	if !sawLockedSelect {
		t.Fatalf("expected SELECT … FOR UPDATE on policy_drafts, captured queries: %v", cap.snapshot())
	}
}
