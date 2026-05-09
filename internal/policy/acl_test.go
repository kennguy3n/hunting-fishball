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

// TestACL_GlobWildcardsAdjacentToDoubleStar covers the regression
// where a deny rule like `secrets/**/api_keys.*` would silently fail
// to match `secrets/prod/api_keys.json` because the segment after
// `**` was matched with strings.Index/HasSuffix instead of as a glob.
// Fail-open on the deny gate is exactly the wrong direction, so the
// translator now folds the entire pattern into a regex once.
func TestACL_GlobWildcardsAdjacentToDoubleStar(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{PathGlob: "secrets/**/api_keys.*", Action: policy.ACLActionDeny},
		},
	}
	denied := []string{
		"secrets/prod/api_keys.json",
		"secrets/staging/team-a/api_keys.yaml",
		"secrets/api_keys.txt",
	}
	for _, p := range denied {
		v := l.Evaluate(policy.ChunkAttrs{Path: p})
		if v.Allowed {
			t.Fatalf("%q must be denied (deny-rule fail-open regression)", p)
		}
	}
	allowed := []string{
		"secrets/prod/passwords.json", // wrong basename
		"public/api_keys.json",        // wrong root
	}
	for _, p := range allowed {
		v := l.Evaluate(policy.ChunkAttrs{Path: p})
		if !v.Allowed {
			t.Fatalf("%q must default-allow", p)
		}
	}
}

// TestACL_GlobSingleStarDoesNotCrossSlash anchors the segment-bounded
// semantics of `*`: a single star must NOT cross a `/`, otherwise
// every `*.md` rule would also match nested paths. The `**` form is
// what crosses segments.
func TestACL_GlobSingleStarDoesNotCrossSlash(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{PathGlob: "docs/*/draft.md", Action: policy.ACLActionDeny},
		},
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "docs/v1/draft.md"}); v.Allowed {
		t.Fatal("docs/*/draft.md must match docs/v1/draft.md")
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "docs/v1/sub/draft.md"}); !v.Allowed {
		t.Fatal("docs/*/draft.md must NOT match docs/v1/sub/draft.md (`*` should not cross `/`)")
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "docs/draft.md"}); !v.Allowed {
		t.Fatal("docs/*/draft.md must NOT match docs/draft.md (`*` matches one segment, not zero)")
	}
}

// TestACL_GlobLeadingDoubleStar covers the `**/...` form: a leading
// `**` must match zero-or-more leading segments so a pattern like
// `**/.env` catches both top-level and nested files.
func TestACL_GlobLeadingDoubleStar(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{PathGlob: "**/.env", Action: policy.ACLActionDeny},
		},
	}
	for _, p := range []string{".env", "app/.env", "app/config/dev/.env"} {
		if v := l.Evaluate(policy.ChunkAttrs{Path: p}); v.Allowed {
			t.Fatalf("%q must be denied by **/.env", p)
		}
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: ".env.local"}); !v.Allowed {
		t.Fatal("**/.env must NOT match .env.local")
	}
}

// TestACL_GlobLiteralRegexMetacharacters guards against pattern
// translation bugs by feeding paths that contain regex metacharacters.
// `.`, `+`, `(`, `)`, etc. must be matched as literals, not regex
// operators.
func TestACL_GlobLiteralRegexMetacharacters(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{PathGlob: "logs/(prod)/app.log", Action: policy.ACLActionDeny},
		},
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "logs/(prod)/app.log"}); v.Allowed {
		t.Fatal("literal `(prod)` must match literally (regex metachars escaped)")
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "logs/prod/app.log"}); !v.Allowed {
		t.Fatal("literal `(prod)` must NOT match `prod` (regex grouping leaked)")
	}
}

// TestACL_GlobCharacterClass covers the `[abc]` / `[!abc]` forms,
// including the negation prefix that the translator rewrites to the
// regex `[^...]` form.
func TestACL_GlobCharacterClass(t *testing.T) {
	t.Parallel()
	l := policy.AllowDenyList{
		Rules: []policy.ACLRule{
			{PathGlob: "logs/[abc].log", Action: policy.ACLActionDeny},
			{PathGlob: "logs/[!xyz].log", Action: policy.ACLActionDeny},
		},
	}
	denied := []string{"logs/a.log", "logs/b.log", "logs/m.log"}
	for _, p := range denied {
		if v := l.Evaluate(policy.ChunkAttrs{Path: p}); v.Allowed {
			t.Fatalf("%q must be denied (character class)", p)
		}
	}
	if v := l.Evaluate(policy.ChunkAttrs{Path: "logs/x.log"}); !v.Allowed {
		t.Fatal("logs/[!xyz].log must NOT match logs/x.log (negation)")
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
