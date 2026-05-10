// Package eval implements the retrieval evaluation harness that
// answers "is retrieval actually any good" — one of the five core
// capabilities of the context engine called out in PROPOSAL.md §1.
//
// An EvalSuite is a labelled corpus of (query, expected chunk IDs)
// tuples that operators run against the live retrieval handler to
// score Precision@K, Recall@K, MRR, and NDCG. The output is an
// EvalReport summarised per-case and aggregated across the suite.
//
// The package is deliberately decoupled from cmd/api so suites can
// be exercised from unit tests, the admin handler, or a CLI
// replayer with the same code paths.
package eval

import (
	"errors"
	"strings"
	"time"
)

// ErrEmptySuite is returned by the runner when the supplied EvalSuite
// has no cases. An empty run produces no signal so we surface it as
// an explicit error instead of silently returning a zero-value report.
var ErrEmptySuite = errors.New("eval: suite has no cases")

// EvalCase is a single labelled query in an EvalSuite. ExpectedChunkIDs
// is the ground-truth set the retriever should return; ExpectedMinScore
// is an optional gate on the top-1 hit's score (0 disables the check).
//
// TopK overrides the suite-wide default for this case; 0 falls back to
// EvalSuite.DefaultK or, ultimately, the package default 10.
type EvalCase struct {
	// Query is the natural-language input passed to the retriever.
	Query string `json:"query"`

	// ExpectedChunkIDs lists the canonical chunk identifiers that
	// represent the gold-set answer for Query. The runner compares
	// the retrieved IDs against this set to compute precision/recall.
	ExpectedChunkIDs []string `json:"expected_chunk_ids"`

	// ExpectedMinScore is the minimum score the top-1 hit must
	// achieve for the case to be considered "scored". Zero disables
	// the check — useful for ranking-only evaluation.
	ExpectedMinScore float32 `json:"expected_min_score,omitempty"`

	// TopK overrides the suite-level K for this case. Zero means
	// use the suite default.
	TopK int `json:"top_k,omitempty"`

	// SkillID is forwarded to the retriever so the recipient gate
	// is exercised the same way it would be in production.
	SkillID string `json:"skill_id,omitempty"`

	// PrivacyMode pins the privacy mode used for this case. Empty
	// uses the retriever's default.
	PrivacyMode string `json:"privacy_mode,omitempty"`
}

// EvalSuite is the corpus an evaluation pass runs against. Persisted
// as a row in eval_suites (migrations/007_eval_suites.sql) for the
// admin portal; callers may also build one in-memory for tests.
type EvalSuite struct {
	Name     string     `json:"name"`
	TenantID string     `json:"tenant_id"`
	Cases    []EvalCase `json:"cases"`
	DefaultK int        `json:"default_k,omitempty"`
}

// Validate reports the first invariant violation in the suite, or nil
// when the suite is well-formed.
func (s EvalSuite) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("eval: suite Name is required")
	}
	if strings.TrimSpace(s.TenantID) == "" {
		return errors.New("eval: suite TenantID is required")
	}
	if len(s.Cases) == 0 {
		return ErrEmptySuite
	}
	for i, c := range s.Cases {
		if strings.TrimSpace(c.Query) == "" {
			return errors.New("eval: case[" + itoa(i) + "] missing Query")
		}
	}
	return nil
}

// EffectiveK resolves the K used for a single case: case override
// wins over suite default, with a final fallback to the package
// default of 10.
func (s EvalSuite) EffectiveK(c EvalCase) int {
	if c.TopK > 0 {
		return c.TopK
	}
	if s.DefaultK > 0 {
		return s.DefaultK
	}
	return defaultK
}

// EvalReport is the runner's output. Cases preserves per-case detail
// so operators can drill into regressions; Aggregate is the average
// across all scored cases.
type EvalReport struct {
	SuiteName   string           `json:"suite_name"`
	TenantID    string           `json:"tenant_id"`
	StartedAt   time.Time        `json:"started_at"`
	CompletedAt time.Time        `json:"completed_at"`
	Duration    time.Duration    `json:"duration"`
	K           int              `json:"k"`
	Cases       []EvalCaseResult `json:"cases"`
	Aggregate   Metrics          `json:"aggregate"`
	// FailedCases counts cases the retriever errored on. Their
	// metrics are zero — they still count against the aggregate
	// to surface regressions instead of papering over them.
	FailedCases int `json:"failed_cases"`
}

// EvalCaseResult is the per-case outcome.
type EvalCaseResult struct {
	Query    string        `json:"query"`
	Expected []string      `json:"expected"`
	Returned []string      `json:"returned"`
	K        int           `json:"k"`
	Metrics  Metrics       `json:"metrics"`
	TopScore float32       `json:"top_score"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

// defaultK is the default top-K when no other value is supplied.
const defaultK = 10

// itoa is a tiny zero-alloc int → string for error message indices.
// Importing strconv just for this would inflate the build graph.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}
