package policy_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// fakeResolver returns a fixed snapshot. Used to provide the live
// policy view to the simulator without standing up Postgres.
type fakeResolver struct {
	snap policy.PolicySnapshot
	err  error
}

func (f *fakeResolver) Resolve(_ context.Context, _, _ string) (policy.PolicySnapshot, error) {
	if f.err != nil {
		return policy.PolicySnapshot{}, f.err
	}
	return f.snap, nil
}

// retrieverWithCorpus simulates a retrieval pipeline by running a
// fixed corpus through the supplied snapshot's ACL gate. Hits whose
// path matches a deny rule are dropped; hits whose privacy_label is
// stricter than the snapshot's effective mode are also dropped. This
// is a faithful approximation of the production handler's
// applyPolicySnapshot + privacy-label filter without booting Qdrant.
func retrieverWithCorpus(corpus []policy.RetrieveHit) policy.RetrieverFunc {
	return func(_ context.Context, req policy.SimRetrieveRequest, snap policy.PolicySnapshot) ([]policy.RetrieveHit, error) {
		out := make([]policy.RetrieveHit, 0, len(corpus))
		for _, h := range corpus {
			if h.TenantID != "" && h.TenantID != req.TenantID {
				continue
			}
			if snap.ACL != nil {
				v := snap.ACL.Evaluate(policy.ChunkAttrs{
					SourceID: h.SourceID, NamespaceID: h.NamespaceID, Path: h.Path,
				})
				if !v.Allowed {
					continue
				}
			}
			if snap.Recipient != nil && !snap.Recipient.IsAllowed(req.SkillID) {
				continue
			}
			if !privacyAllows(snap.EffectiveMode, h.PrivacyLabel) {
				continue
			}
			out = append(out, h)
		}
		if req.TopK > 0 && len(out) > req.TopK {
			out = out[:req.TopK]
		}
		return out, nil
	}
}

// privacyAllows mirrors the retrieval handler's privacy-label filter
// in its strictness ordering. Empty mode == permissive.
func privacyAllows(mode policy.PrivacyMode, label string) bool {
	if mode == "" {
		return true
	}
	if mode == policy.PrivacyModeNoAI {
		return false
	}
	return policy.AllowsAtLeast(mode, policy.PrivacyMode(label))
}

func TestSimulator_WhatIf_DiffsAddedAndRemoved(t *testing.T) {
	t.Parallel()
	corpus := []policy.RetrieveHit{
		{ID: "h1", TenantID: "tenant-a", PrivacyLabel: "local-only", Path: "drive/public/readme.md"},
		{ID: "h2", TenantID: "tenant-a", PrivacyLabel: "local-only", Path: "drive/secret/payroll.csv"},
		{ID: "h3", TenantID: "tenant-a", PrivacyLabel: "local-only", Path: "drive/public/handbook.md"},
	}

	// Live policy: deny everything under drive/secret. Draft policy
	// drops the deny — h2 should appear in DraftResults but not
	// LiveResults.
	live := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			TenantID: "tenant-a",
			Rules: []policy.ACLRule{
				{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
			},
		},
	}
	draft := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL:           &policy.AllowDenyList{TenantID: "tenant-a"},
	}

	s, err := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: &fakeResolver{snap: live},
		Retriever:    retrieverWithCorpus(corpus),
	})
	if err != nil {
		t.Fatalf("NewSimulator: %v", err)
	}
	res, err := s.WhatIf(context.Background(), policy.WhatIfRequest{
		TenantID: "tenant-a", Query: "x", DraftPolicy: draft,
	})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if len(res.Added) != 1 || res.Added[0].ID != "h2" {
		t.Fatalf("Added: %+v", res.Added)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed: %+v", res.Removed)
	}
}

func TestSimulator_WhatIf_DiffsRemoved(t *testing.T) {
	t.Parallel()
	corpus := []policy.RetrieveHit{
		{ID: "h1", TenantID: "tenant-a", PrivacyLabel: "local-only", Path: "drive/a"},
		{ID: "h2", TenantID: "tenant-a", PrivacyLabel: "local-only", Path: "drive/b"},
	}
	live := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL:           &policy.AllowDenyList{TenantID: "tenant-a"},
	}
	// Draft tightens: deny everything under drive/.
	draft := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			TenantID: "tenant-a",
			Rules: []policy.ACLRule{
				{PathGlob: "drive/**", Action: policy.ACLActionDeny},
			},
		},
	}

	s, _ := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: &fakeResolver{snap: live},
		Retriever:    retrieverWithCorpus(corpus),
	})
	res, err := s.WhatIf(context.Background(), policy.WhatIfRequest{
		TenantID: "tenant-a", Query: "x", DraftPolicy: draft,
	})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("Removed: %+v", res.Removed)
	}
	if len(res.Added) != 0 {
		t.Fatalf("Added: %+v", res.Added)
	}
	got := []string{res.Removed[0].ID, res.Removed[1].ID}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"h1", "h2"}) {
		t.Fatalf("Removed IDs: %v", got)
	}
}

func TestSimulator_WhatIf_ChangedScoresAndLabels(t *testing.T) {
	t.Parallel()
	// The retriever stamps a different score and privacy_label for
	// the same chunk depending on whether the snapshot has any ACL
	// rules — that mimics the rerank-and-relabel behaviour the real
	// pipeline applies after a policy change.
	retriever := func(_ context.Context, req policy.SimRetrieveRequest, snap policy.PolicySnapshot) ([]policy.RetrieveHit, error) {
		score := float32(0.5)
		label := "local-only"
		if snap.ACL != nil && len(snap.ACL.Rules) > 0 {
			score = 0.9
			label = "local-api"
		}
		return []policy.RetrieveHit{
			{ID: "h1", TenantID: req.TenantID, Score: score, PrivacyLabel: label},
		}, nil
	}
	live := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}
	draft := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{{Action: policy.ACLActionAllow}},
		},
	}
	s, _ := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: &fakeResolver{snap: live},
		Retriever:    retriever,
	})
	res, err := s.WhatIf(context.Background(), policy.WhatIfRequest{
		TenantID: "tenant-a", Query: "x", DraftPolicy: draft,
	})
	if err != nil {
		t.Fatalf("WhatIf: %v", err)
	}
	if len(res.Changed) != 1 {
		t.Fatalf("Changed: %+v", res.Changed)
	}
	cd := res.Changed[0]
	if cd.LiveScore != 0.5 || cd.DraftScore != 0.9 {
		t.Fatalf("scores: %+v", cd)
	}
	if cd.LivePrivacyLabel != "local-only" || cd.DraftPrivacyLabel != "local-api" {
		t.Fatalf("labels: %+v", cd)
	}
}

func TestSimulator_WhatIf_ResolverErrorPropagates(t *testing.T) {
	t.Parallel()
	s, _ := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: &fakeResolver{err: errors.New("boom")},
		Retriever:    retrieverWithCorpus(nil),
	})
	_, err := s.WhatIf(context.Background(), policy.WhatIfRequest{TenantID: "t", Query: "q"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSimulator_WhatIf_RequiresTenantAndQuery(t *testing.T) {
	t.Parallel()
	s, _ := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: &fakeResolver{},
		Retriever:    retrieverWithCorpus(nil),
	})
	if _, err := s.WhatIf(context.Background(), policy.WhatIfRequest{Query: "q"}); err == nil {
		t.Fatal("expected TenantID error")
	}
	if _, err := s.WhatIf(context.Background(), policy.WhatIfRequest{TenantID: "t"}); err == nil {
		t.Fatal("expected Query error")
	}
}

func TestNewSimulator_Validation(t *testing.T) {
	t.Parallel()
	if _, err := policy.NewSimulator(policy.SimulatorConfig{Retriever: retrieverWithCorpus(nil)}); err == nil {
		t.Fatal("expected LiveResolver error")
	}
	if _, err := policy.NewSimulator(policy.SimulatorConfig{LiveResolver: &fakeResolver{}}); err == nil {
		t.Fatal("expected Retriever error")
	}
}

func TestPolicySnapshot_Clone_DoesNotAliasRules(t *testing.T) {
	t.Parallel()
	src := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeHybrid,
		ACL: &policy.AllowDenyList{
			TenantID: "tenant-a",
			Rules: []policy.ACLRule{
				{PathGlob: "a/**", Action: policy.ACLActionAllow},
			},
		},
		Recipient: &policy.RecipientPolicy{
			TenantID: "tenant-a",
			Rules:    []policy.RecipientRule{{SkillID: "qa", Action: policy.ACLActionAllow}},
		},
	}
	clone := src.Clone()
	clone.ACL.Rules = append(clone.ACL.Rules, policy.ACLRule{PathGlob: "b/**", Action: policy.ACLActionDeny})
	clone.Recipient.Rules = append(clone.Recipient.Rules, policy.RecipientRule{SkillID: "x", Action: policy.ACLActionDeny})

	if len(src.ACL.Rules) != 1 {
		t.Fatalf("clone leaked into src.ACL: %d rules", len(src.ACL.Rules))
	}
	if len(src.Recipient.Rules) != 1 {
		t.Fatalf("clone leaked into src.Recipient: %d rules", len(src.Recipient.Rules))
	}
}

func TestPolicySnapshot_Clone_DeepCopiesChunkACL(t *testing.T) {
	// Regression for the Round-6 ChunkACL leak: the new
	// PolicySnapshot.ChunkACL pointer was not deep-copied in
	// Clone(), so concurrent what-if retrievals and draft
	// promotions shared a mutable ACL with the live snapshot.
	t.Parallel()
	src := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeHybrid,
		ChunkACL: policy.NewChunkACL([]policy.ChunkACLTag{
			{ChunkID: "a", Decision: policy.ChunkACLDecisionDeny},
		}),
	}
	clone := src.Clone()

	// Mutating the clone must not bleed back into src.
	clone.ChunkACL.Add(policy.ChunkACLTag{ChunkID: "b", Decision: policy.ChunkACLDecisionDeny})
	if src.ChunkACL.Len() != 1 {
		t.Fatalf("clone leaked into src.ChunkACL: %d rules", src.ChunkACL.Len())
	}
	if clone.ChunkACL.Len() != 2 {
		t.Fatalf("clone must own 2 rules; got %d", clone.ChunkACL.Len())
	}
	if src.ChunkACL == clone.ChunkACL {
		t.Fatal("clone.ChunkACL must be a distinct pointer from src.ChunkACL")
	}
}

func TestPolicySnapshot_Clone_NilChunkACL(t *testing.T) {
	t.Parallel()
	src := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}
	if clone := src.Clone(); clone.ChunkACL != nil {
		t.Fatalf("nil src.ChunkACL must clone to nil; got %v", clone.ChunkACL)
	}
}
