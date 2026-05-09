package policy_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestACL_EmptyListAllowsByDefault(t *testing.T) {
	t.Parallel()
	var l policy.AllowDenyList
	v := l.Evaluate(policy.ChunkAttrs{SourceID: "s", NamespaceID: "n", Path: "p"})
	if !v.Allowed {
		t.Fatal("empty list must default-allow")
	}
	if v.MatchedRule != -1 {
		t.Fatalf("MatchedRule: %d", v.MatchedRule)
	}
}

func TestACL_DenyOverridesAllow(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{SourceID: "src-1", Action: policy.ACLActionAllow},
			{SourceID: "src-1", NamespaceID: "ns-1", Action: policy.ACLActionDeny},
		},
	}
	got := l.Evaluate(policy.ChunkAttrs{SourceID: "src-1", NamespaceID: "ns-1"})
	if got.Allowed {
		t.Fatalf("deny must win, got %+v", got)
	}
}

func TestACL_PerSourceAndNamespaceFiltering(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{SourceID: "src-public", Action: policy.ACLActionAllow, ComputeTier: "light"},
			{SourceID: "src-private", Action: policy.ACLActionDeny},
		},
	}
	pub := l.Evaluate(policy.ChunkAttrs{SourceID: "src-public"})
	if !pub.Allowed || pub.ComputeTier != "light" {
		t.Fatalf("public source: %+v", pub)
	}
	priv := l.Evaluate(policy.ChunkAttrs{SourceID: "src-private"})
	if priv.Allowed {
		t.Fatalf("private source must be denied, got %+v", priv)
	}
}

func TestACL_GlobMatching(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{PathGlob: "secrets/*.txt", Action: policy.ACLActionDeny},
			{PathGlob: "docs/**", Action: policy.ACLActionAllow},
		},
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "secrets/keys.txt"}); v.Allowed {
		t.Fatal("secrets/keys.txt must be denied")
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "docs/handbook/intro.md"}); !v.Allowed {
		t.Fatal("docs/** must allow nested paths")
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "code/main.go"}); !v.Allowed {
		t.Fatal("default-allow on no match")
	}
}

func TestACL_FirstMatchingAllowComputeTier(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{SourceID: "s", Action: policy.ACLActionAllow, ComputeTier: "first"},
			{SourceID: "s", Action: policy.ACLActionAllow, ComputeTier: "second"},
		},
	}
	v := l.Evaluate(policy.ChunkAttrs{SourceID: "s"})
	if !v.Allowed || v.ComputeTier != "first" {
		t.Fatalf("expected first tier, got %+v", v)
	}
}

func TestACL_NilListAllows(t *testing.T) {
	t.Parallel()
	var l *policy.AllowDenyList
	v := l.Evaluate(policy.ChunkAttrs{SourceID: "s"})
	if !v.Allowed {
		t.Fatal("nil list must default-allow")
	}
}

func TestACL_SortRules_DenyFirst(t *testing.T) {
	t.Parallel()
	rules := []policy.ACLRule{
		{SourceID: "z", Action: policy.ACLActionAllow},
		{SourceID: "a", Action: policy.ACLActionAllow},
		{SourceID: "m", Action: policy.ACLActionDeny},
	}
	got := policy.SortRules(rules)
	if got[0].Action != policy.ACLActionDeny {
		t.Fatalf("deny rule must come first, got %+v", got)
	}
	if got[1].SourceID != "a" || got[2].SourceID != "z" {
		t.Fatalf("alphabetical secondary sort broken: %+v", got)
	}
	// SortRules must NOT mutate the input slice.
	if rules[0].SourceID != "z" {
		t.Fatal("SortRules mutated input")
	}
}
