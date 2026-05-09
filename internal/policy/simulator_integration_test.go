package policy_test

import (
	"context"
	"sort"
	"testing"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// Integration tests for the full Phase 4 simulator flow:
//
//  1. An admin drafts a tighter policy.
//  2. The simulator runs what-if and data-flow diffs against the
//     known corpus and reports the expected added/removed chunks.
//  3. Conflict detection passes on the clean draft.
//  4. Conflict detection blocks promotion of a deliberately
//     conflicting draft.
//  5. The promotion workflow flips the draft to promoted, applies
//     the snapshot to the (mock) live store, and emits the
//     policy.promoted audit event.
//  6. A subsequent retrieval against the live store returns the
//     same hits the simulator predicted.
//
// The scenario uses three corpus chunks:
//
//   - drive/public/intro.md      (privacy_label "remote")
//   - drive/public/spec.md       (privacy_label "local-api")
//   - drive/secret/payroll.csv   (privacy_label "remote")
//
// The live policy is permissive (allow drive/**). The draft policy
// keeps the allow but adds a deny for drive/secret/**, which should
// drop the third chunk from the result set.

type integrationCorpus struct{}

type integrationChunk struct {
	id, path, label string
	score           float32
}

func newIntegrationCorpus() []integrationChunk {
	return []integrationChunk{
		{id: "intro", path: "drive/public/intro.md", label: "remote", score: 0.9},
		{id: "spec", path: "drive/public/spec.md", label: "local-api", score: 0.8},
		{id: "payroll", path: "drive/secret/payroll.csv", label: "remote", score: 0.7},
	}
}

// integrationRetriever runs the corpus through the supplied
// snapshot's ACL gate. It is the test harness for the simulator —
// it does NOT use the real retrieval handler (which has too many
// moving parts for an in-package test), but it mirrors the gating
// behaviour exactly.
func integrationRetriever(corpus []integrationChunk) policy.RetrieverFunc {
	return func(_ context.Context, _ policy.SimRetrieveRequest, snapshot policy.PolicySnapshot) ([]policy.RetrieveHit, error) {
		out := []policy.RetrieveHit{}
		for _, c := range corpus {
			if !aclAllows(snapshot.ACL, c.path) {
				continue
			}
			out = append(out, policy.RetrieveHit{
				ID: c.id, Score: c.score, PrivacyLabel: c.label,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
		return out, nil
	}
}

func aclAllows(acl *policy.AllowDenyList, path string) bool {
	if acl == nil {
		return true
	}
	return acl.Evaluate(policy.ChunkAttrs{Path: path}).Allowed
}

func livePermissive() policy.PolicySnapshot {
	return policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow},
		}},
	}
}

// liveStoreApplied is a recording LiveStore that captures the
// snapshot the promotion workflow applies, plus exposes a Resolve
// method so a "post-promotion retrieval" can verify the live state.
type liveStoreApplied struct {
	current policy.PolicySnapshot
}

func (l *liveStoreApplied) ApplySnapshot(_ context.Context, _ *gorm.DB, _, _ string, snap policy.PolicySnapshot) error {
	l.current = snap.Clone()
	return nil
}

func (l *liveStoreApplied) Resolve(_ context.Context, _, _ string) (policy.PolicySnapshot, error) {
	return l.current.Clone(), nil
}

func TestSimulatorIntegration_FullFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	corpus := newIntegrationCorpus()
	live := &liveStoreApplied{current: livePermissive()}

	// 1. Admin drafts a tighter policy: deny drive/secret/**.
	draftSnap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow},
			{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
		}},
	}

	repo, _ := newSQLiteDraftRepo(t)
	d := policy.NewDraft("tenant-a", "", "ken", draftSnap)
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create draft: %v", err)
	}

	// 2. Wire a Simulator whose LiveResolver reads from the live
	// store and whose Retriever runs the corpus through the
	// supplied snapshot.
	sim, err := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: policy.ResolverFunc(live.Resolve),
		Retriever:    integrationRetriever(corpus),
	})
	if err != nil {
		t.Fatalf("NewSimulator: %v", err)
	}

	whatif := policy.WhatIfRequest{
		TenantID:    "tenant-a",
		Query:       "what is the payroll process",
		TopK:        10,
		DraftPolicy: draftSnap,
	}

	// 3. WhatIf: live returns 3 chunks, draft drops the secret one.
	res, err := sim.WhatIf(ctx, whatif)
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if len(res.LiveResults) != 3 {
		t.Fatalf("LiveResults: %d (want 3)", len(res.LiveResults))
	}
	if len(res.DraftResults) != 2 {
		t.Fatalf("DraftResults: %d (want 2)", len(res.DraftResults))
	}
	if len(res.Removed) != 1 || res.Removed[0].ID != "payroll" {
		t.Fatalf("Removed: %+v (want [payroll])", res.Removed)
	}
	if len(res.Added) != 0 {
		t.Fatalf("Added: %+v (want [])", res.Added)
	}

	// 4. Data-flow diff: live = {remote:2, local-api:1};
	//    draft = {remote:1, local-api:1}.
	flow, err := sim.SimulateDataFlow(ctx, whatif)
	if err != nil {
		t.Fatalf("SimulateDataFlow: %v", err)
	}
	if flow.Live["remote"] != 2 || flow.Live["local-api"] != 1 {
		t.Fatalf("Live counts: %+v", flow.Live)
	}
	if flow.Draft["remote"] != 1 || flow.Draft["local-api"] != 1 {
		t.Fatalf("Draft counts: %+v", flow.Draft)
	}
	if flow.Delta["remote"] != -1 {
		t.Fatalf("Delta[remote]: %d (want -1)", flow.Delta["remote"])
	}

	// 5. Conflict detection on the clean draft: no errors.
	conflicts := policy.DetectConflicts(draftSnap)
	if policy.HasErrors(conflicts) {
		t.Fatalf("clean draft must not have error conflicts: %+v", conflicts)
	}

	// 6. Conflict detection on a deliberately conflicting draft.
	conflictingSnap := draftSnap.Clone()
	conflictingSnap.ACL.Rules = append(conflictingSnap.ACL.Rules,
		policy.ACLRule{PathGlob: "drive/**", Action: policy.ACLActionDeny},
	)
	if !policy.HasErrors(policy.DetectConflicts(conflictingSnap)) {
		t.Fatalf("conflicting draft must produce errors")
	}

	// 7. Promote the clean draft. The promoter wires the draft
	// repository, the live store, and a fake audit recorder; this
	// exercises the full transactional path.
	ad := &fakeAudit{}
	prom, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts:    repo,
		LiveStore: live,
		Audit:     ad,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	promoted, err := prom.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1")
	if err != nil {
		t.Fatalf("PromoteDraft: %v", err)
	}
	if promoted.Status != policy.DraftStatusPromoted {
		t.Fatalf("promoted status: %q", promoted.Status)
	}
	actions := ad.actions()
	if len(actions) != 1 || actions[0] != audit.ActionPolicyPromoted {
		t.Fatalf("audit actions: %v", actions)
	}

	// 8. Post-promotion retrieval: the live store now serves the
	// draft's snapshot, so the simulator's retriever (run against
	// live.Resolve) returns the same two chunks WhatIf predicted.
	postLive, err := sim.WhatIf(ctx, policy.WhatIfRequest{
		TenantID:    "tenant-a",
		Query:       "what is the payroll process",
		TopK:        10,
		DraftPolicy: live.current,
	})
	if err != nil {
		t.Fatalf("post-promotion WhatIf: %v", err)
	}
	if len(postLive.LiveResults) != 2 {
		t.Fatalf("post-promotion LiveResults: %d (want 2)", len(postLive.LiveResults))
	}
	if len(postLive.Added)+len(postLive.Removed)+len(postLive.Changed) != 0 {
		t.Fatalf("post-promotion diff should be empty: added=%v removed=%v changed=%v",
			postLive.Added, postLive.Removed, postLive.Changed)
	}
}

func TestSimulatorIntegration_PromotionBlockedByConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	live := &liveStoreApplied{current: livePermissive()}
	conflictingSnap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow},
			{PathGlob: "drive/**", Action: policy.ACLActionDeny},
		}},
	}

	repo, _ := newSQLiteDraftRepo(t)
	d := policy.NewDraft("tenant-a", "", "ken", conflictingSnap)
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("Create draft: %v", err)
	}

	ad := &fakeAudit{}
	prom, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts:    repo,
		LiveStore: live,
		Audit:     ad,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}

	_, err = prom.PromoteDraft(ctx, "tenant-a", d.ID, "actor-1")
	if err == nil {
		t.Fatal("expected promotion to be blocked")
	}
	// Live store must NOT have changed.
	if !aclAllows(live.current.ACL, "drive/anything.md") {
		t.Fatalf("live store mutated despite blocked promotion")
	}
	// No audit event for a blocked promotion.
	if len(ad.actions()) != 0 {
		t.Fatalf("audit must be silent on blocked promotion: %v", ad.actions())
	}
}
