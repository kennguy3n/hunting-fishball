package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestBuildDataFlowDiff_TighteningPolicy(t *testing.T) {
	t.Parallel()
	live := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "remote"},
		{ID: "b", PrivacyLabel: "remote"},
		{ID: "c", PrivacyLabel: "remote"},
		{ID: "d", PrivacyLabel: "remote"},
		{ID: "e", PrivacyLabel: "local-api"},
	}
	// Draft demotes 3 of 4 remote hits down to local-only.
	draft := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "local-only"},
		{ID: "b", PrivacyLabel: "local-only"},
		{ID: "c", PrivacyLabel: "local-only"},
		{ID: "d", PrivacyLabel: "remote"},
		{ID: "e", PrivacyLabel: "local-api"},
	}
	d := policy.BuildDataFlowDiff(live, draft)
	if d.Live["remote"] != 4 || d.Draft["remote"] != 1 {
		t.Fatalf("remote counts: live=%d draft=%d", d.Live["remote"], d.Draft["remote"])
	}
	if d.Delta["remote"] != -3 {
		t.Fatalf("Delta[remote]: %d", d.Delta["remote"])
	}
	if d.Delta["local-only"] != 3 {
		t.Fatalf("Delta[local-only]: %d", d.Delta["local-only"])
	}
	if d.Summary == "" {
		t.Fatalf("Summary should be non-empty")
	}
	if !strings.Contains(d.Summary, "fewer") {
		t.Fatalf("Summary should mention fewer (tightening): %q", d.Summary)
	}
}

func TestBuildDataFlowDiff_LooseningPolicy(t *testing.T) {
	t.Parallel()
	live := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "local-only"},
		{ID: "b", PrivacyLabel: "local-only"},
	}
	draft := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "local-only"},
		{ID: "b", PrivacyLabel: "local-only"},
		{ID: "c", PrivacyLabel: "remote"},
		{ID: "d", PrivacyLabel: "remote"},
	}
	d := policy.BuildDataFlowDiff(live, draft)
	if d.Delta["remote"] != 2 {
		t.Fatalf("Delta[remote]: %d", d.Delta["remote"])
	}
	if !strings.Contains(d.Summary, "more") {
		t.Fatalf("Summary should mention more (loosening): %q", d.Summary)
	}
}

func TestBuildDataFlowDiff_NoChange(t *testing.T) {
	t.Parallel()
	hits := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "remote"},
		{ID: "b", PrivacyLabel: "local-only"},
	}
	d := policy.BuildDataFlowDiff(hits, hits)
	if d.Delta["remote"] != 0 || d.Delta["local-only"] != 0 {
		t.Fatalf("Delta should be zero: %+v", d.Delta)
	}
	if d.Summary != "" {
		t.Fatalf("Summary should be empty: %q", d.Summary)
	}
}

func TestBuildDataFlowDiff_UnknownLabel(t *testing.T) {
	t.Parallel()
	live := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: ""},
		{ID: "b", PrivacyLabel: "local-only"},
	}
	draft := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "local-only"},
		{ID: "b", PrivacyLabel: "local-only"},
	}
	d := policy.BuildDataFlowDiff(live, draft)
	if d.Live["unknown"] != 1 {
		t.Fatalf("unknown bucket: %d", d.Live["unknown"])
	}
	if d.Draft["unknown"] != 0 {
		t.Fatalf("draft unknown bucket: %d", d.Draft["unknown"])
	}
}

func TestBuildDataFlowDiff_PercentInSummary(t *testing.T) {
	t.Parallel()
	live := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "remote"},
		{ID: "b", PrivacyLabel: "remote"},
		{ID: "c", PrivacyLabel: "remote"},
		{ID: "d", PrivacyLabel: "remote"},
	}
	draft := []policy.RetrieveHit{
		{ID: "a", PrivacyLabel: "local-only"},
		{ID: "b", PrivacyLabel: "local-only"},
		{ID: "c", PrivacyLabel: "remote"},
		{ID: "d", PrivacyLabel: "remote"},
	}
	d := policy.BuildDataFlowDiff(live, draft)
	if !strings.Contains(d.Summary, "%") {
		t.Fatalf("Summary should include a percentage: %q", d.Summary)
	}
}

func TestSimulator_SimulateDataFlow_AggregatesAcrossSnapshots(t *testing.T) {
	t.Parallel()
	corpus := []policy.RetrieveHit{
		{ID: "h1", TenantID: "tenant-a", PrivacyLabel: "remote", Path: "drive/secret/a"},
		{ID: "h2", TenantID: "tenant-a", PrivacyLabel: "remote", Path: "drive/secret/b"},
		{ID: "h3", TenantID: "tenant-a", PrivacyLabel: "local-only", Path: "drive/public/c"},
	}
	live := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL:           &policy.AllowDenyList{TenantID: "tenant-a"},
	}
	// Draft: deny everything under drive/secret. Both remote chunks
	// disappear; only the local-only one survives.
	draft := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			TenantID: "tenant-a",
			Rules: []policy.ACLRule{
				{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
			},
		},
	}
	s, err := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: &fakeResolver{snap: live},
		Retriever:    retrieverWithCorpus(corpus),
	})
	if err != nil {
		t.Fatalf("NewSimulator: %v", err)
	}
	d, err := s.SimulateDataFlow(context.Background(), policy.WhatIfRequest{
		TenantID: "tenant-a", Query: "x", DraftPolicy: draft,
	})
	if err != nil {
		t.Fatalf("SimulateDataFlow: %v", err)
	}
	if d.LiveTotal != 3 || d.DraftTotal != 1 {
		t.Fatalf("totals: live=%d draft=%d", d.LiveTotal, d.DraftTotal)
	}
	if d.Delta["remote"] != -2 {
		t.Fatalf("Delta[remote]: %d", d.Delta["remote"])
	}
}

func TestSimulator_SimulateDataFlow_RequiresInputs(t *testing.T) {
	t.Parallel()
	s, _ := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: &fakeResolver{},
		Retriever:    retrieverWithCorpus(nil),
	})
	if _, err := s.SimulateDataFlow(context.Background(), policy.WhatIfRequest{Query: "q"}); err == nil {
		t.Fatal("expected TenantID error")
	}
	if _, err := s.SimulateDataFlow(context.Background(), policy.WhatIfRequest{TenantID: "t"}); err == nil {
		t.Fatal("expected Query error")
	}
}
