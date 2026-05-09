package retrieval_test

import (
	"context"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

func TestLinearReranker_FreshnessLifts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rer := retrieval.NewLinearReranker(retrieval.LinearRerankerConfig{
		FusionWeight:           0,
		BM25Weight:             0,
		FreshnessWeight:        1,
		FreshnessHalfLifeHours: 24,
		Now:                    func() time.Time { return now },
	})
	matches := []*retrieval.Match{
		{ID: "old", Source: retrieval.SourceVector, IngestedAt: now.AddDate(0, 0, -7)},
		{ID: "new", Source: retrieval.SourceVector, IngestedAt: now},
	}
	out, err := rer.Rerank(context.Background(), "q", matches)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if out[0].ID != "new" {
		t.Fatalf("fresh chunk should top, got order %v", []string{out[0].ID, out[1].ID})
	}
	if out[0].Score <= out[1].Score {
		t.Fatalf("fresh score should exceed old: %v vs %v", out[0].Score, out[1].Score)
	}
}

func TestLinearReranker_BlendsBM25Score(t *testing.T) {
	t.Parallel()

	rer := retrieval.NewLinearReranker(retrieval.LinearRerankerConfig{
		FusionWeight:    1,
		BM25Weight:      1,
		FreshnessWeight: 0,
	})
	now := time.Now()
	matches := []*retrieval.Match{
		{ID: "a", Source: retrieval.SourceBM25, Sources: []string{retrieval.SourceBM25}, Score: 10, IngestedAt: now},
		{ID: "b", Source: retrieval.SourceVector, Sources: []string{retrieval.SourceVector}, Score: 1, IngestedAt: now},
	}
	out, err := rer.Rerank(context.Background(), "q", matches)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if out[0].ID != "a" {
		t.Fatalf("BM25 hit should top: %v", []string{out[0].ID, out[1].ID})
	}
}

func TestLinearReranker_DefaultsAreSane(t *testing.T) {
	t.Parallel()

	rer := retrieval.NewLinearReranker(retrieval.LinearRerankerConfig{})
	out, err := rer.Rerank(context.Background(), "q", []*retrieval.Match{
		{ID: "a", Source: retrieval.SourceVector, Score: 0.5},
	})
	if err != nil || len(out) != 1 {
		t.Fatalf("Rerank: %v %d", err, len(out))
	}
}

func TestLinearReranker_RespectsContextCancel(t *testing.T) {
	t.Parallel()

	rer := retrieval.NewLinearReranker(retrieval.LinearRerankerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rer.Rerank(ctx, "q", []*retrieval.Match{{ID: "a"}}); err == nil {
		t.Fatalf("expected context error")
	}
}

func TestPolicyFilter_DropsAboveChannelMode(t *testing.T) {
	t.Parallel()

	pf := retrieval.NewPolicyFilter(retrieval.PolicyConfig{})
	matches := []*retrieval.Match{
		{ID: "a", PrivacyLabel: "public"},
		{ID: "b", PrivacyLabel: "internal"},
		{ID: "c", PrivacyLabel: "confidential"},
		{ID: "d", PrivacyLabel: "restricted"},
		{ID: "e", PrivacyLabel: "secret"},
	}
	res := pf.Apply(matches, "internal")
	if len(res.Allowed) != 2 || res.Allowed[0].ID != "a" || res.Allowed[1].ID != "b" {
		t.Fatalf("allowed: %+v", res.Allowed)
	}
	if res.BlockedCount != 3 {
		t.Fatalf("blocked count: %d", res.BlockedCount)
	}
	if len(res.BlockedLabels) != 3 {
		t.Fatalf("blocked labels: %v", res.BlockedLabels)
	}
}

func TestPolicyFilter_UnknownLabelIsBlocked(t *testing.T) {
	t.Parallel()

	pf := retrieval.NewPolicyFilter(retrieval.PolicyConfig{})
	res := pf.Apply([]*retrieval.Match{
		{ID: "a", PrivacyLabel: "public"},
		{ID: "b", PrivacyLabel: "highly-classified"},
	}, "secret")
	if len(res.Allowed) != 1 || res.Allowed[0].ID != "a" {
		t.Fatalf("allowed: %+v", res.Allowed)
	}
	if res.BlockedCount != 1 {
		t.Fatalf("blocked count: %d", res.BlockedCount)
	}
}

func TestPolicyFilter_EmptyChannelDefaultsToMostRestrictive(t *testing.T) {
	t.Parallel()

	pf := retrieval.NewPolicyFilter(retrieval.PolicyConfig{})
	res := pf.Apply([]*retrieval.Match{
		{ID: "a", PrivacyLabel: "public"},
		{ID: "b", PrivacyLabel: "internal"},
	}, "")
	if len(res.Allowed) != 1 || res.Allowed[0].ID != "a" {
		t.Fatalf("expected only public to pass: %+v", res.Allowed)
	}
}

func TestPolicyFilter_EmptyLabelDefaultsToPublic(t *testing.T) {
	t.Parallel()

	pf := retrieval.NewPolicyFilter(retrieval.PolicyConfig{})
	res := pf.Apply([]*retrieval.Match{
		{ID: "a", PrivacyLabel: ""},
	}, "public")
	if len(res.Allowed) != 1 {
		t.Fatalf("expected unlabelled chunk to default to public: %+v", res)
	}
}

func TestPolicyFilter_CustomOrder(t *testing.T) {
	t.Parallel()

	pf := retrieval.NewPolicyFilter(retrieval.PolicyConfig{
		PrivacyOrder: []string{"low", "medium", "high"},
	})
	res := pf.Apply([]*retrieval.Match{
		{ID: "a", PrivacyLabel: "low"},
		{ID: "b", PrivacyLabel: "high"},
	}, "medium")
	if len(res.Allowed) != 1 || res.Allowed[0].ID != "a" {
		t.Fatalf("custom order broken: %+v", res.Allowed)
	}
}
