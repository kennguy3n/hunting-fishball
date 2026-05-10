//go:build eval

// Package eval_ci wires `tests/eval/golden_corpus.json` into the
// CI pipeline. Run via `make eval` (which sets the `eval` build
// tag) — the test loads the corpus, drives `internal/eval.Run`
// against in-memory fakes that mirror the four production
// backends (vector / bm25 / graph / memory), and asserts the
// aggregate metrics stay above the thresholds embedded in the
// corpus file. Drops below the floor → CI fails.
//
// The fakes intentionally model "perfect retrieval" for cases
// the corpus marks as belonging to that backend (so the gold-set
// itself doesn't measure backend wiring — that's what the
// integration suite covers). The point of this test is to pin
// regressions in the runner / metrics / aggregation code: if a
// future change alters how Run scores a perfect match, the
// thresholds catch it.
package eval_ci

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/eval"
)

// goldenCorpus mirrors the JSON file shape. We embed `eval.EvalCase`
// fields explicitly so the on-disk file can carry extra
// documentation hints (`backend`, `description`) without having to
// teach the eval package about them.
type goldenCorpus struct {
	Name        string           `json:"name"`
	TenantID    string           `json:"tenant_id"`
	Description string           `json:"description"`
	DefaultK    int              `json:"default_k"`
	Thresholds  goldenThresholds `json:"thresholds"`
	Cases       []goldenCase     `json:"cases"`
}

type goldenThresholds struct {
	MinPrecisionAtK float64 `json:"min_precision_at_k"`
	MinRecallAtK    float64 `json:"min_recall_at_k"`
	MinMRR          float64 `json:"min_mrr"`
	MinNDCG         float64 `json:"min_ndcg"`
	MaxFailedCases  int     `json:"max_failed_cases"`
}

type goldenCase struct {
	Backend          string   `json:"backend"`
	Query            string   `json:"query"`
	ExpectedChunkIDs []string `json:"expected_chunk_ids"`
	ExpectedMinScore float32  `json:"expected_min_score,omitempty"`
	SkillID          string   `json:"skill_id,omitempty"`
	PrivacyMode      string   `json:"privacy_mode,omitempty"`
}

// goldenRetriever is the in-memory fake. It returns the
// expected_chunk_ids for the matching query at a score that meets
// the per-case expected_min_score, so the runner reports perfect
// metrics. This is deliberate: the eval CI gate is a regression
// guard on the runner / metrics, NOT on the live backend stack
// (that's tested by `tests/integration/`).
type goldenRetriever struct {
	cases map[string]goldenCase
}

func newGoldenRetriever(cases []goldenCase) *goldenRetriever {
	out := &goldenRetriever{cases: make(map[string]goldenCase, len(cases))}
	for _, c := range cases {
		out.cases[c.Query] = c
	}
	return out
}

func (g *goldenRetriever) Retrieve(_ context.Context, req eval.RetrieveRequest) ([]eval.RetrieveHit, error) {
	c, ok := g.cases[req.Query]
	if !ok {
		return nil, nil
	}
	score := c.ExpectedMinScore
	if score == 0 {
		score = 0.9
	}
	hits := make([]eval.RetrieveHit, 0, len(c.ExpectedChunkIDs))
	for i, id := range c.ExpectedChunkIDs {
		// Decay slightly so subsequent hits don't tie the top
		// score; runner aggregation should still credit them.
		hits = append(hits, eval.RetrieveHit{ID: id, Score: score - float32(i)*0.001})
	}
	return hits, nil
}

func loadCorpus(t *testing.T) goldenCorpus {
	t.Helper()
	path := filepath.Join("golden_corpus.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden_corpus.json: %v", err)
	}
	var c goldenCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal golden_corpus.json: %v", err)
	}
	if c.Name == "" || c.TenantID == "" {
		t.Fatalf("golden corpus missing name or tenant_id")
	}
	if len(c.Cases) < 15 {
		t.Fatalf("golden corpus must have >= 15 cases, got %d", len(c.Cases))
	}
	return c
}

// TestGoldenCorpus_AggregateAboveThresholds is the headline CI
// gate: the corpus is loaded, the runner drives the perfect
// in-memory retriever, and the aggregate metrics must clear
// every floor declared in the corpus. The job in `.github/
// workflows/ci.yml` runs this with `-tags=eval`.
func TestGoldenCorpus_AggregateAboveThresholds(t *testing.T) {
	t.Parallel()
	corpus := loadCorpus(t)

	cases := make([]eval.EvalCase, 0, len(corpus.Cases))
	for _, c := range corpus.Cases {
		cases = append(cases, eval.EvalCase{
			Query:            c.Query,
			ExpectedChunkIDs: c.ExpectedChunkIDs,
			ExpectedMinScore: c.ExpectedMinScore,
			SkillID:          c.SkillID,
			PrivacyMode:      c.PrivacyMode,
		})
	}
	suite := eval.EvalSuite{
		Name:     corpus.Name,
		TenantID: corpus.TenantID,
		Cases:    cases,
		DefaultK: corpus.DefaultK,
	}

	report, err := eval.Run(context.Background(), newGoldenRetriever(corpus.Cases), suite)
	if err != nil {
		t.Fatalf("eval.Run: %v", err)
	}

	if report.FailedCases > corpus.Thresholds.MaxFailedCases {
		t.Fatalf("failed_cases=%d exceeds max=%d (cases=%v)", report.FailedCases, corpus.Thresholds.MaxFailedCases, summarise(report))
	}
	agg := report.Aggregate
	if float64(agg.PrecisionAtK) < corpus.Thresholds.MinPrecisionAtK {
		t.Fatalf("PrecisionAtK regression: got %.4f, threshold %.4f", agg.PrecisionAtK, corpus.Thresholds.MinPrecisionAtK)
	}
	if float64(agg.RecallAtK) < corpus.Thresholds.MinRecallAtK {
		t.Fatalf("RecallAtK regression: got %.4f, threshold %.4f", agg.RecallAtK, corpus.Thresholds.MinRecallAtK)
	}
	if float64(agg.MRR) < corpus.Thresholds.MinMRR {
		t.Fatalf("MRR regression: got %.4f, threshold %.4f", agg.MRR, corpus.Thresholds.MinMRR)
	}
	if float64(agg.NDCG) < corpus.Thresholds.MinNDCG {
		t.Fatalf("NDCG regression: got %.4f, threshold %.4f", agg.NDCG, corpus.Thresholds.MinNDCG)
	}
}

// TestGoldenCorpus_HasPerBackendCoverage guards the corpus shape
// itself: every fan-out backend must have at least one case so
// adding a new backend to the runner can't silently leave it
// unevaluated.
func TestGoldenCorpus_HasPerBackendCoverage(t *testing.T) {
	t.Parallel()
	corpus := loadCorpus(t)
	want := map[string]bool{"vector": false, "bm25": false, "graph": false, "memory": false}
	for _, c := range corpus.Cases {
		if _, ok := want[c.Backend]; ok {
			want[c.Backend] = true
		}
	}
	for backend, present := range want {
		if !present {
			t.Errorf("golden corpus missing case for backend=%q", backend)
		}
	}
}

func summarise(r *eval.EvalReport) string {
	out, _ := json.Marshal(struct {
		Aggregate   eval.Metrics `json:"aggregate"`
		FailedCases int          `json:"failed_cases"`
		Total       int          `json:"total"`
	}{r.Aggregate, r.FailedCases, len(r.Cases)})
	return string(out)
}
