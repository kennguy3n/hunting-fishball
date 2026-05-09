package policy_test

import (
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestDetectConflicts_Empty(t *testing.T) {
	t.Parallel()
	got := policy.DetectConflicts(policy.PolicySnapshot{})
	if len(got) != 0 {
		t.Fatalf("expected no conflicts, got %+v", got)
	}
}

func TestDetectConflicts_NoAIWithACLIsWarning(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeNoAI,
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{{PathGlob: "x", Action: policy.ACLActionAllow}},
		},
	}
	got := policy.DetectConflicts(snap)
	if len(got) != 1 {
		t.Fatalf("expected one conflict, got %+v", got)
	}
	if got[0].Severity != policy.ConflictSeverityWarning {
		t.Fatalf("severity: %q", got[0].Severity)
	}
	if got[0].Type != policy.ConflictTypePrivacyModeOverride {
		t.Fatalf("type: %q", got[0].Type)
	}
	if policy.HasErrors(got) {
		t.Fatal("HasErrors should be false for warning-only set")
	}
}

func TestDetectConflicts_NoAIWithoutACLIsClean(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeNoAI}
	if got := policy.DetectConflicts(snap); len(got) != 0 {
		t.Fatalf("expected no conflicts for bare no-ai snapshot, got %+v", got)
	}
}

func TestDetectConflicts_ACLIdenticalScopeContradictionIsError(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{PathGlob: "drive/**", Action: policy.ACLActionAllow},
				{PathGlob: "drive/**", Action: policy.ACLActionDeny},
			},
		},
	}
	got := policy.DetectConflicts(snap)
	if len(got) == 0 {
		t.Fatalf("expected at least one conflict")
	}
	var sawError bool
	for _, c := range got {
		if c.Severity == policy.ConflictSeverityError && c.Type == policy.ConflictTypeACLOverlap {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected acl_overlap error, got %+v", got)
	}
	if !policy.HasErrors(got) {
		t.Fatal("HasErrors should be true")
	}
}

func TestDetectConflicts_ACLNestedGlobIsWarning(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{PathGlob: "drive/**", Action: policy.ACLActionAllow},
				{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
			},
		},
	}
	got := policy.DetectConflicts(snap)
	if len(got) != 1 {
		t.Fatalf("expected exactly one conflict, got %+v", got)
	}
	if got[0].Severity != policy.ConflictSeverityWarning {
		t.Fatalf("severity: %q", got[0].Severity)
	}
	if got[0].Type != policy.ConflictTypeACLOverlap {
		t.Fatalf("type: %q", got[0].Type)
	}
	if !strings.Contains(got[0].Description, "nested") {
		t.Fatalf("description should mention nested: %q", got[0].Description)
	}
}

func TestDetectConflicts_DisjointSourcesNoConflict(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{SourceID: "src-1", PathGlob: "x", Action: policy.ACLActionAllow},
				{SourceID: "src-2", PathGlob: "x", Action: policy.ACLActionDeny},
			},
		},
	}
	if got := policy.DetectConflicts(snap); len(got) != 0 {
		t.Fatalf("expected no conflicts for disjoint sources, got %+v", got)
	}
}

func TestDetectConflicts_RecipientCatchAllDenyVsExplicitAllowIsError(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		Recipient: &policy.RecipientPolicy{
			Rules: []policy.RecipientRule{
				{SkillID: "", Action: policy.ACLActionDeny},
				{SkillID: "qa", Action: policy.ACLActionAllow},
			},
		},
	}
	got := policy.DetectConflicts(snap)
	var sawError bool
	for _, c := range got {
		if c.Severity == policy.ConflictSeverityError && c.Type == policy.ConflictTypeRecipientContradiction {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected recipient_contradiction error: %+v", got)
	}
}

func TestDetectConflicts_RecipientCatchAllAllowVsExplicitDenyIsWarning(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		Recipient: &policy.RecipientPolicy{
			Rules: []policy.RecipientRule{
				{SkillID: "", Action: policy.ACLActionAllow},
				{SkillID: "qa", Action: policy.ACLActionDeny},
			},
		},
	}
	got := policy.DetectConflicts(snap)
	if len(got) != 1 {
		t.Fatalf("expected exactly one conflict, got %+v", got)
	}
	if got[0].Severity != policy.ConflictSeverityWarning {
		t.Fatalf("severity: %q", got[0].Severity)
	}
	if policy.HasErrors(got) {
		t.Fatal("HasErrors should be false")
	}
}

func TestDetectConflicts_DeterministicOrdering(t *testing.T) {
	t.Parallel()
	snap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{PathGlob: "a", Action: policy.ACLActionAllow},
				{PathGlob: "a", Action: policy.ACLActionDeny},
				{PathGlob: "b/**", Action: policy.ACLActionAllow},
				{PathGlob: "b/sub/**", Action: policy.ACLActionDeny},
			},
		},
	}
	first := policy.DetectConflicts(snap)
	second := policy.DetectConflicts(snap)
	if len(first) != len(second) {
		t.Fatalf("non-deterministic length: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("non-deterministic at idx %d: %+v vs %+v", i, first[i], second[i])
		}
	}
	// Errors must precede warnings.
	for i := 0; i < len(first)-1; i++ {
		if first[i].Severity == policy.ConflictSeverityWarning && first[i+1].Severity == policy.ConflictSeverityError {
			t.Fatalf("error must precede warning at idx %d: %+v / %+v", i, first[i], first[i+1])
		}
	}
}
