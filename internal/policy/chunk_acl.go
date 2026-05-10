package policy

// chunk_acl.go — Round-6 Task 6.
//
// Per-chunk ACL tags allow operators to explicitly grant or deny
// retrieval access to individual chunks (or ranges of chunks)
// regardless of the source-level AllowDenyList. This is the
// "needle-in-the-haystack" override: a single chunk inside an
// otherwise-allowed source can be denied (e.g. PII slip through),
// or a single chunk inside a denied source can be allow-listed.
//
// Evaluation order in the retrieval handler:
//
//  1. AllowDenyList (source-level) — already implemented.
//  2. ChunkACL.Evaluate (this file) — applies per-chunk tags.
//
// Storage: migrations/020_chunk_acl.sql holds (chunk_id, tag,
// allow/deny). The retrieval policy snapshot loader hydrates a
// ChunkACL into the snapshot.

import (
	"strings"
	"sync"
)

// ChunkACLDecision enumerates the per-chunk verdict.
type ChunkACLDecision int

const (
	// ChunkACLDecisionAllow means the chunk is explicitly allowed
	// by a tag rule (or no rule matched and the default is allow).
	ChunkACLDecisionAllow ChunkACLDecision = iota
	// ChunkACLDecisionDeny means an explicit deny rule fired.
	ChunkACLDecisionDeny
)

// ChunkACLTag is a single per-chunk tag rule. Either ChunkID OR
// TagPrefix matches against the chunk; ChunkID is the exact match
// and TagPrefix is a string-prefix match against any tag attached
// to the chunk via the source's metadata.
type ChunkACLTag struct {
	// ChunkID is the exact chunk_id this rule targets. Empty when
	// the rule matches by TagPrefix instead.
	ChunkID string

	// TagPrefix matches any chunk tag that starts with this
	// prefix. Empty when the rule matches by exact ChunkID.
	TagPrefix string

	// Decision is the verdict for matching chunks.
	Decision ChunkACLDecision
}

// ChunkACL is the read-side container the policy snapshot owns. It
// is safe for concurrent reads after construction; writers must
// hold the package-level mutex.
type ChunkACL struct {
	mu   sync.RWMutex
	rows []ChunkACLTag
}

// NewChunkACL constructs a ChunkACL with the supplied rules.
func NewChunkACL(rules []ChunkACLTag) *ChunkACL {
	c := &ChunkACL{rows: append([]ChunkACLTag(nil), rules...)}
	return c
}

// Add appends rules to the ACL atomically.
func (c *ChunkACL) Add(rules ...ChunkACLTag) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, rules...)
}

// Len returns the number of rules.
func (c *ChunkACL) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.rows)
}

// ChunkACLAttrs is the per-chunk shape Evaluate inspects. The
// retrieval handler projects retrieval.Match → ChunkACLAttrs
// (chunk ID + the chunk's tag set drawn from metadata).
type ChunkACLAttrs struct {
	ChunkID string
	Tags    []string
}

// Evaluate returns the verdict for attrs. Semantics mirror
// AllowDenyList: deny wins, default-allow on no match.
func (c *ChunkACL) Evaluate(attrs ChunkACLAttrs) ChunkACLDecision {
	if c == nil {
		return ChunkACLDecisionAllow
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.rows) == 0 {
		return ChunkACLDecisionAllow
	}
	hasAllow := false
	for _, r := range c.rows {
		if !chunkRuleMatches(r, attrs) {
			continue
		}
		if r.Decision == ChunkACLDecisionDeny {
			return ChunkACLDecisionDeny
		}
		hasAllow = true
	}
	_ = hasAllow
	return ChunkACLDecisionAllow
}

func chunkRuleMatches(r ChunkACLTag, attrs ChunkACLAttrs) bool {
	if r.ChunkID != "" && r.ChunkID == attrs.ChunkID {
		return true
	}
	if r.TagPrefix == "" {
		return false
	}
	for _, t := range attrs.Tags {
		if strings.HasPrefix(t, r.TagPrefix) {
			return true
		}
	}
	return false
}
