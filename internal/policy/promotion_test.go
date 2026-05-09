package policy_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// fakeAudit captures audit logs and runs the same Validate() check
// the production repository does, so a promotion path that builds an
// AuditLog without an ID fails the test instead of silently passing
// through a slice append. Mirrors internal/admin/source_handler_test.go.
//
// fakeAudit also captures the *gorm.DB CreateInTx was called with so
// tests can assert audit writes ride the same tx as the rest of the
// promotion (the transactional-outbox guarantee).
type fakeAudit struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
	txs  []*gorm.DB
}

func (f *fakeAudit) Create(_ context.Context, log *audit.AuditLog) error {
	return f.record(nil, log)
}

func (f *fakeAudit) CreateInTx(_ context.Context, tx *gorm.DB, log *audit.AuditLog) error {
	if tx == nil {
		return errors.New("fakeAudit: nil tx")
	}
	return f.record(tx, log)
}

func (f *fakeAudit) record(tx *gorm.DB, log *audit.AuditLog) error {
	if log == nil {
		return errors.New("fakeAudit: nil log")
	}
	if err := log.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, log)
	f.txs = append(f.txs, tx)
	return nil
}

func (f *fakeAudit) actions() []audit.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.Action, 0, len(f.logs))
	for _, l := range f.logs {
		out = append(out, l.Action)
	}
	return out
}

func (f *fakeAudit) lastTx() *gorm.DB {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.txs) == 0 {
		return nil
	}
	return f.txs[len(f.txs)-1]
}

// fakeLiveStore records ApplySnapshot calls and lets a test return a
// canned error to exercise the rollback path. It also captures the
// *gorm.DB tx it was handed so the AuditUsesTx test can assert the
// audit and live-store writes share the outer transaction.
type fakeLiveStore struct {
	mu       sync.Mutex
	called   int
	lastSnap policy.PolicySnapshot
	lastTx   *gorm.DB
	err      error
}

func (f *fakeLiveStore) ApplySnapshot(_ context.Context, tx *gorm.DB, _, _ string, snap policy.PolicySnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.lastSnap = snap
	f.lastTx = tx
	return f.err
}

func newPromoter(t *testing.T) (*policy.Promoter, *policy.DraftRepository, *fakeLiveStore, *fakeAudit) {
	t.Helper()
	repo, _ := newSQLiteDraftRepo(t)
	live := &fakeLiveStore{}
	a := &fakeAudit{}
	p, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts:    repo,
		LiveStore: live,
		Audit:     a,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	return p, repo, live, a
}

func TestPromoter_PromoteDraft_HappyPath(t *testing.T) {
	t.Parallel()
	p, repo, live, ad := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "channel-1", "ken", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{TenantID: "tenant-a", Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow},
		}},
	})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1")
	if err != nil {
		t.Fatalf("PromoteDraft: %v", err)
	}
	if got.Status != policy.DraftStatusPromoted {
		t.Fatalf("status: %q", got.Status)
	}
	if got.PromotedBy != "actor-1" {
		t.Fatalf("PromotedBy: %q", got.PromotedBy)
	}
	if live.called != 1 {
		t.Fatalf("ApplySnapshot calls: %d", live.called)
	}
	if live.lastSnap.EffectiveMode != policy.PrivacyModeRemote {
		t.Fatalf("snapshot not propagated: %+v", live.lastSnap)
	}
	actions := ad.actions()
	if len(actions) != 1 || actions[0] != audit.ActionPolicyPromoted {
		t.Fatalf("audit actions: %v", actions)
	}
}

func TestPromoter_PromoteDraft_BlocksOnErrorConflict(t *testing.T) {
	t.Parallel()
	p, repo, live, ad := newPromoter(t)
	ctx := context.Background()

	// Deliberately conflicting ACL: identical scope, opposing actions.
	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			TenantID: "tenant-a",
			Rules: []policy.ACLRule{
				{PathGlob: "drive/**", Action: policy.ACLActionAllow},
				{PathGlob: "drive/**", Action: policy.ACLActionDeny},
			},
		},
	})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1")
	if err == nil {
		t.Fatal("expected ErrPromotionBlocked")
	}
	var blocked *policy.ErrPromotionBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("error type: %T %v", err, err)
	}
	if len(blocked.Conflicts) == 0 {
		t.Fatal("blocked.Conflicts should be populated")
	}

	// Live tables must NOT be mutated when promotion is blocked.
	if live.called != 0 {
		t.Fatalf("ApplySnapshot must not run when blocked: %d", live.called)
	}
	if len(ad.actions()) != 0 {
		t.Fatalf("no audit event should fire when blocked: %v", ad.actions())
	}
	// Draft must remain in 'draft' status so the admin can edit
	// and re-attempt.
	got, err := repo.Get(ctx, "tenant-a", d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != policy.DraftStatusDraft {
		t.Fatalf("status: %q (should remain draft)", got.Status)
	}
}

func TestPromoter_PromoteDraft_RollsBackOnLiveStoreError(t *testing.T) {
	t.Parallel()
	p, repo, live, ad := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	live.err = errors.New("postgres unreachable")

	if _, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1"); err == nil {
		t.Fatal("expected error")
	}
	got, _ := repo.Get(ctx, "tenant-a", d.ID)
	if got.Status != policy.DraftStatusDraft {
		t.Fatalf("status should roll back to draft: %q", got.Status)
	}
	if len(ad.actions()) != 0 {
		t.Fatalf("no audit event should fire on rollback: %v", ad.actions())
	}
}

func TestPromoter_PromoteDraft_RejectsTerminalDraft(t *testing.T) {
	t.Parallel()
	p, repo, _, _ := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1"); err != nil {
		t.Fatalf("first PromoteDraft: %v", err)
	}
	_, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1")
	if !errors.Is(err, policy.ErrDraftTerminal) {
		t.Fatalf("expected ErrDraftTerminal, got %v", err)
	}
}

func TestPromoter_PromoteDraft_TenantIsolation(t *testing.T) {
	t.Parallel()
	p, repo, _, _ := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := p.PromoteDraft(ctx, "tenant-b", d.ID, "actor-1"); !errors.Is(err, policy.ErrDraftNotFound) {
		t.Fatalf("expected ErrDraftNotFound, got %v", err)
	}
}

func TestPromoter_RejectDraft_HappyPath(t *testing.T) {
	t.Parallel()
	p, repo, live, ad := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := p.RejectDraft(ctx, "tenant-a", d.ID, "actor-1", "out-of-date")
	if err != nil {
		t.Fatalf("RejectDraft: %v", err)
	}
	if got.Status != policy.DraftStatusRejected {
		t.Fatalf("status: %q", got.Status)
	}
	if got.RejectReason != "out-of-date" {
		t.Fatalf("RejectReason: %q", got.RejectReason)
	}
	if live.called != 0 {
		t.Fatal("ApplySnapshot must not run on reject")
	}
	actions := ad.actions()
	if len(actions) != 1 || actions[0] != audit.ActionPolicyRejected {
		t.Fatalf("audit actions: %v", actions)
	}
}

func TestPromoter_RejectDraft_RejectsTerminal(t *testing.T) {
	t.Parallel()
	p, repo, _, _ := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1"); err != nil {
		t.Fatalf("PromoteDraft: %v", err)
	}
	_, err := p.RejectDraft(ctx, "tenant-a", d.ID, "actor-1", "")
	if !errors.Is(err, policy.ErrDraftTerminal) {
		t.Fatalf("expected ErrDraftTerminal, got %v", err)
	}
}

// TestPromoter_PromoteDraft_AuditWritesUseOuterTx is the regression
// test for the transactional-outbox bug: the audit row MUST be
// inserted via CreateInTx (riding the closure's tx), not via Create
// (which would use the repository's default DB handle and survive an
// outer-tx rollback).
//
// The test asserts that the *gorm.DB pointer captured by the audit
// writer equals the pointer captured by the live store. Both are
// supposed to be the tx GORM hands to the Transaction closure, so
// equality proves the audit insert is enrolled in the same tx as
// the live-store write — which is exactly what CreateInTx
// guarantees and Create does not.
func TestPromoter_PromoteDraft_AuditWritesUseOuterTx(t *testing.T) {
	t.Parallel()
	p, repo, live, ad := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
	})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create draft: %v", err)
	}

	if _, err := p.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1"); err != nil {
		t.Fatalf("PromoteDraft: %v", err)
	}

	if live.lastTx == nil {
		t.Fatal("fakeLiveStore did not capture a tx")
	}
	auditTx := ad.lastTx()
	if auditTx == nil {
		t.Fatal("fakeAudit did not capture a tx (CreateInTx not called?)")
	}
	if auditTx != live.lastTx {
		t.Fatalf("audit tx %p != live-store tx %p — audit did not ride the outer tx", auditTx, live.lastTx)
	}
}

// TestPromoter_RejectDraft_AuditWritesUseOuterTx mirrors the
// PromoteDraft regression test for the rejection path.
func TestPromoter_RejectDraft_AuditWritesUseOuterTx(t *testing.T) {
	t.Parallel()
	p, repo, _, ad := newPromoter(t)
	ctx := context.Background()

	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{})
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create draft: %v", err)
	}

	if _, err := p.RejectDraft(ctx, "tenant-a", d.ID, "actor-1", "stale"); err != nil {
		t.Fatalf("RejectDraft: %v", err)
	}

	if ad.lastTx() == nil {
		t.Fatal("fakeAudit did not capture a tx (CreateInTx not called?)")
	}
}

func TestNewPromoter_Validation(t *testing.T) {
	t.Parallel()
	if _, err := policy.NewPromoter(policy.PromotionConfig{}); err == nil {
		t.Fatal("expected error for missing Drafts")
	}
	repo, _ := newSQLiteDraftRepo(t)
	if _, err := policy.NewPromoter(policy.PromotionConfig{Drafts: repo}); err == nil {
		t.Fatal("expected error for missing LiveStore")
	}
	if _, err := policy.NewPromoter(policy.PromotionConfig{Drafts: repo, LiveStore: &fakeLiveStore{}}); err == nil {
		t.Fatal("expected error for missing Audit")
	}
}
