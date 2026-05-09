package retrieval_test

import (
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

func TestBuildPrivacyStrip_RemoteMode(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{
		ID: "h1", PrivacyLabel: "remote", Connector: "google_drive",
	}
	snap := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}
	got := retrieval.BuildPrivacyStrip(m, snap)
	if got.Mode != "remote" {
		t.Fatalf("Mode: %q", got.Mode)
	}
	if got.ProcessedWhere != "remote" {
		t.Fatalf("ProcessedWhere: %q", got.ProcessedWhere)
	}
	if got.ModelTier != "remote-llm" {
		t.Fatalf("ModelTier: %q", got.ModelTier)
	}
	if len(got.DataSources) != 1 || got.DataSources[0] != "google_drive" {
		t.Fatalf("DataSources: %+v", got.DataSources)
	}
}

func TestBuildPrivacyStrip_LocalOnlyMode(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{ID: "h1", PrivacyLabel: "local-only", Connector: "slack"}
	snap := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeLocalOnly}
	got := retrieval.BuildPrivacyStrip(m, snap)
	if got.ProcessedWhere != "on-device" {
		t.Fatalf("ProcessedWhere: %q", got.ProcessedWhere)
	}
	if got.ModelTier != "local-slm" {
		t.Fatalf("ModelTier: %q", got.ModelTier)
	}
}

func TestBuildPrivacyStrip_NoAIModeBlocks(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{ID: "h1", PrivacyLabel: "no-ai"}
	snap := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeNoAI}
	got := retrieval.BuildPrivacyStrip(m, snap)
	if got.ProcessedWhere != "blocked" {
		t.Fatalf("ProcessedWhere: %q", got.ProcessedWhere)
	}
	if got.ModelTier != "none" {
		t.Fatalf("ModelTier: %q", got.ModelTier)
	}
}

func TestBuildPrivacyStrip_HybridMode(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{ID: "h1", PrivacyLabel: "hybrid", Connector: "google_drive"}
	snap := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeHybrid}
	got := retrieval.BuildPrivacyStrip(m, snap)
	if got.ProcessedWhere != "local-api" {
		t.Fatalf("ProcessedWhere: %q", got.ProcessedWhere)
	}
	if got.ModelTier != "remote-llm" {
		t.Fatalf("ModelTier: %q", got.ModelTier)
	}
}

func TestBuildPrivacyStrip_EmptyLabelFallsBackToMode(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{ID: "h1", PrivacyLabel: ""}
	snap := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeLocalAPI}
	got := retrieval.BuildPrivacyStrip(m, snap)
	if got.ProcessedWhere != "local-api" {
		t.Fatalf("ProcessedWhere: %q", got.ProcessedWhere)
	}
	if got.ModelTier != "local-slm" {
		t.Fatalf("ModelTier: %q", got.ModelTier)
	}
}

func TestBuildPrivacyStrip_DataSourcesFallbackToBackends(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{
		ID: "h1", PrivacyLabel: "remote",
		Sources: []string{"vector", "bm25"},
	}
	snap := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote}
	got := retrieval.BuildPrivacyStrip(m, snap)
	if len(got.DataSources) != 2 {
		t.Fatalf("DataSources: %+v", got.DataSources)
	}
	// Sorted output for determinism.
	if got.DataSources[0] != "bm25" || got.DataSources[1] != "vector" {
		t.Fatalf("DataSources order: %+v", got.DataSources)
	}
}

func TestBuildPrivacyStrip_NilMatch(t *testing.T) {
	t.Parallel()
	got := retrieval.BuildPrivacyStrip(nil, policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote})
	if got.Mode != "remote" {
		t.Fatalf("Mode: %q", got.Mode)
	}
	if got.ProcessedWhere != "unknown" {
		t.Fatalf("ProcessedWhere: %q", got.ProcessedWhere)
	}
	if got.ModelTier != "none" {
		t.Fatalf("ModelTier: %q", got.ModelTier)
	}
}

func TestBuildPrivacyStrip_PolicyAppliedSurfacesACLRule(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{
		ID: "h1", PrivacyLabel: "remote", Connector: "google_drive",
		Metadata: map[string]any{"path": "drive/secret/payroll.csv"},
	}
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{PathGlob: "drive/secret/**", Action: policy.ACLActionAllow, ComputeTier: "local-api"},
			},
		},
	}
	got := retrieval.BuildPrivacyStrip(m, snap)
	if len(got.PolicyApplied) == 0 {
		t.Fatalf("PolicyApplied should include matched rule, got %+v", got.PolicyApplied)
	}
	if !strings.HasPrefix(got.PolicyApplied[0], "acl[0]:") {
		t.Fatalf("rule ref format: %q", got.PolicyApplied[0])
	}
}

func TestBuildPrivacyStrip_PolicyAppliedSurfacesRecipientPolicy(t *testing.T) {
	t.Parallel()
	m := &retrieval.Match{ID: "h1", PrivacyLabel: "remote"}
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		Recipient: &policy.RecipientPolicy{
			Rules: []policy.RecipientRule{{SkillID: "qa", Action: policy.ACLActionAllow}},
		},
	}
	got := retrieval.BuildPrivacyStrip(m, snap)
	var sawRecipient bool
	for _, ref := range got.PolicyApplied {
		if ref == "recipient_policy:active" {
			sawRecipient = true
		}
	}
	if !sawRecipient {
		t.Fatalf("PolicyApplied should mention recipient policy, got %+v", got.PolicyApplied)
	}
}

func TestBuildPrivacyStrip_EmptyModeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	got := retrieval.BuildPrivacyStrip(&retrieval.Match{ID: "h"}, policy.PolicySnapshot{})
	if got.Mode != "unknown" {
		t.Fatalf("Mode: %q", got.Mode)
	}
}
