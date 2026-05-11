package policy_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestChunkACL_NilDefaultAllow(t *testing.T) {
	var c *policy.ChunkACL
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "x"}); d != policy.ChunkACLDecisionAllow {
		t.Fatalf("nil ACL must default-allow; got %v", d)
	}
}

func TestChunkACL_EmptyDefaultAllow(t *testing.T) {
	c := policy.NewChunkACL(nil)
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "x"}); d != policy.ChunkACLDecisionAllow {
		t.Fatalf("empty ACL must default-allow; got %v", d)
	}
}

func TestChunkACL_ExactDeny(t *testing.T) {
	c := policy.NewChunkACL([]policy.ChunkACLTag{
		{ChunkID: "ch-1", Decision: policy.ChunkACLDecisionDeny},
	})
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "ch-1"}); d != policy.ChunkACLDecisionDeny {
		t.Fatalf("exact deny must fire; got %v", d)
	}
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "ch-2"}); d != policy.ChunkACLDecisionAllow {
		t.Fatalf("non-matching chunk must default-allow; got %v", d)
	}
}

func TestChunkACL_TagPrefixDeny(t *testing.T) {
	c := policy.NewChunkACL([]policy.ChunkACLTag{
		{TagPrefix: "pii.", Decision: policy.ChunkACLDecisionDeny},
	})
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "x", Tags: []string{"pii.ssn"}}); d != policy.ChunkACLDecisionDeny {
		t.Fatalf("pii. deny must fire; got %v", d)
	}
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "x", Tags: []string{"safe"}}); d != policy.ChunkACLDecisionAllow {
		t.Fatalf("non-matching tag must default-allow; got %v", d)
	}
}

func TestChunkACL_DenyWins(t *testing.T) {
	c := policy.NewChunkACL([]policy.ChunkACLTag{
		{TagPrefix: "approved.", Decision: policy.ChunkACLDecisionAllow},
		{ChunkID: "ch-1", Decision: policy.ChunkACLDecisionDeny},
	})
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "ch-1", Tags: []string{"approved.a"}}); d != policy.ChunkACLDecisionDeny {
		t.Fatalf("deny must override allow; got %v", d)
	}
}

func TestChunkACL_InheritanceFromSourceTag(t *testing.T) {
	// Tag inheritance: a chunk inherits the source's permission
	// tags. With a "source.<id>" prefix rule, chunks tagged
	// "source.123" are denied even if their chunk_id has no
	// explicit rule.
	c := policy.NewChunkACL([]policy.ChunkACLTag{
		{TagPrefix: "source.", Decision: policy.ChunkACLDecisionDeny},
	})
	if d := c.Evaluate(policy.ChunkACLAttrs{ChunkID: "x", Tags: []string{"source.legacy"}}); d != policy.ChunkACLDecisionDeny {
		t.Fatalf("inherited deny must fire; got %v", d)
	}
}

func TestChunkACL_CloneIsIndependent(t *testing.T) {
	// Regression for the PolicySnapshot.Clone() leak: mutating
	// the clone (Add) must not bleed into the original ACL, and
	// vice versa.
	orig := policy.NewChunkACL([]policy.ChunkACLTag{
		{ChunkID: "a", Decision: policy.ChunkACLDecisionDeny},
	})
	cl := orig.Clone()
	cl.Add(policy.ChunkACLTag{ChunkID: "b", Decision: policy.ChunkACLDecisionDeny})

	if orig.Len() != 1 {
		t.Fatalf("orig must remain at 1 rule; got %d", orig.Len())
	}
	if cl.Len() != 2 {
		t.Fatalf("clone must hold both rules; got %d", cl.Len())
	}
	// Verdicts must match the rule split.
	if d := orig.Evaluate(policy.ChunkACLAttrs{ChunkID: "b"}); d != policy.ChunkACLDecisionAllow {
		t.Fatalf("orig must allow `b` (rule was added to clone only); got %v", d)
	}
	if d := cl.Evaluate(policy.ChunkACLAttrs{ChunkID: "b"}); d != policy.ChunkACLDecisionDeny {
		t.Fatalf("clone must deny `b`; got %v", d)
	}
}

func TestChunkACL_CloneNilSafe(t *testing.T) {
	var c *policy.ChunkACL
	if got := c.Clone(); got != nil {
		t.Fatalf("nil ChunkACL.Clone() must return nil; got %v", got)
	}
}

func TestChunkACL_AddIsConcurrentSafe(t *testing.T) {
	c := policy.NewChunkACL(nil)
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			c.Add(policy.ChunkACLTag{ChunkID: string(rune('a' + id)), Decision: policy.ChunkACLDecisionDeny})
			_ = c.Evaluate(policy.ChunkACLAttrs{ChunkID: "z"})
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if c.Len() != 8 {
		t.Fatalf("expected 8 rules; got %d", c.Len())
	}
}
