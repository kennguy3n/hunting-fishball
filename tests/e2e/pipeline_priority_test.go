//go:build e2e

// Package e2e — pipeline_priority_test.go — Round-9 Task 14.
//
// End-to-end coverage for the Round-6 priority buffer (Task 5) +
// semantic dedup (Task 2) features. The original Round-6 tests
// cover each component in isolation; this file wires them together
// behind the same env-var flags operators flip in production
// (`CONTEXT_ENGINE_PRIORITY_ENABLED`, `CONTEXT_ENGINE_DEDUP_ENABLED`)
// and asserts the combined behaviour:
//
//  1. With priority enabled, steady-state events (PriorityHigh /
//     EventReindex) drain before backfill (PriorityLow) regardless
//     of producer order.
//  2. With dedup enabled at a 0.95 threshold, a near-duplicate
//     chunk is dropped from the Stage-4 write set.
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// helper: build a 16-dim embedding from a single value, normalised
// so the cosine math is well-defined.
func unitVector(seed float32) []float32 {
	v := make([]float32, 16)
	for i := range v {
		v[i] = seed
	}
	// Normalise to unit length so cosine sim is just a dot product.
	var s float32
	for _, x := range v {
		s += x * x
	}
	if s == 0 {
		return v
	}
	inv := float32(1.0)
	if s != 1 {
		inv = float32(1 / sqrt32(s))
	}
	for i := range v {
		v[i] *= inv
	}
	return v
}

// sqrt32 — inline tiny shim so the test file doesn't need a math
// import for one call.
func sqrt32(x float32) float32 {
	z := x
	for i := 0; i < 20; i++ {
		if z == 0 {
			break
		}
		z = (z + x/z) / 2
	}
	return z
}

// TestPipelinePriority_SteadyBeforeBackfill_E2E flips
// CONTEXT_ENGINE_PRIORITY_ENABLED on, pushes a mix of backfill
// (PriorityLow) and reindex (PriorityHigh) events into a
// PriorityBuffer wired exactly the way cmd/ingest does, and asserts
// the Drain consumer sees the steady-state events first.
func TestPipelinePriority_SteadyBeforeBackfill_E2E(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_PRIORITY_ENABLED", "true")
	if v := os.Getenv("CONTEXT_ENGINE_PRIORITY_ENABLED"); v != "true" {
		t.Fatalf("env not set: %s", v)
	}

	// Defaults match cmd/ingest's wiring. Capacity is generous so
	// the producer never blocks.
	buf := pipeline.NewPriorityBuffer(pipeline.PriorityBufferConfig{
		HighCapacity:   16,
		NormalCapacity: 16,
		LowCapacity:    16,
		NormalAfter:    4,
		LowAfter:       8,
	})
	defer buf.Close()

	classifier := pipeline.DefaultPriorityClassifier{}

	events := []pipeline.IngestEvent{
		// 3 backfills enqueued FIRST.
		{Kind: pipeline.EventDocumentChanged, TenantID: "t-1", SourceID: "src-backfill", DocumentID: "d-b-1"},
		{Kind: pipeline.EventDocumentChanged, TenantID: "t-1", SourceID: "src-backfill", DocumentID: "d-b-2"},
		{Kind: pipeline.EventDocumentChanged, TenantID: "t-1", SourceID: "src-backfill", DocumentID: "d-b-3"},
		// 2 steady reindexes enqueued AFTER. With strict priority
		// the consumer should still pick these up first.
		{Kind: pipeline.EventReindex, TenantID: "t-1", SourceID: "src-steady", DocumentID: "d-s-1"},
		{Kind: pipeline.EventReindex, TenantID: "t-1", SourceID: "src-steady", DocumentID: "d-s-2"},
	}

	// Backfill src is demoted to PriorityLow via overrides.
	classifier.Overrides = map[string]pipeline.Priority{
		"src-backfill": pipeline.PriorityLow,
	}

	for _, ev := range events {
		prio := classifier.Classify(ev)
		if err := buf.Push(context.Background(), ev, prio); err != nil {
			t.Fatalf("push %s: %v", ev.DocumentID, err)
		}
	}

	got := make([]string, 0, len(events))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf.Drain(ctx, func(ev pipeline.IngestEvent) {
			got = append(got, ev.DocumentID)
			if len(got) >= len(events) {
				buf.Close()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("drain hung: got=%v", got)
	}

	if len(got) != len(events) {
		t.Fatalf("got %d events; want %d (%v)", len(got), len(events), got)
	}
	// First two pops must be the steady reindexes (PriorityHigh)
	// despite being enqueued LAST.
	if got[0] != "d-s-1" || got[1] != "d-s-2" {
		t.Fatalf("priority order violated: got %v; want d-s-* first", got)
	}
	// Remaining three are the backfills.
	for i, want := range []string{"d-b-1", "d-b-2", "d-b-3"} {
		if got[2+i] != want {
			t.Fatalf("backfill order: got %v; want %s at position %d", got, want, 2+i)
		}
	}

	highStat, _, lowStat := buf.Stats()
	if highStat != 2 || lowStat != 3 {
		t.Fatalf("stats high=%d low=%d; want 2 / 3", highStat, lowStat)
	}
}

// TestPipelineDedup_DropsNearDuplicate_E2E flips
// CONTEXT_ENGINE_DEDUP_ENABLED on, feeds two near-identical chunk
// embeddings into a Deduplicator wired from
// LoadDedupConfigFromEnv, and asserts the second chunk is dropped.
func TestPipelineDedup_DropsNearDuplicate_E2E(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_DEDUP_ENABLED", "true")
	t.Setenv("CONTEXT_ENGINE_DEDUP_THRESHOLD", "0.95")
	cfg := pipeline.LoadDedupConfigFromEnv()
	if !cfg.Enabled {
		t.Fatalf("env not parsed: %#v", cfg)
	}
	d := pipeline.NewDeduplicator(cfg)

	blocks := []pipeline.Block{
		{BlockID: "b-1", Text: "Acme Corp launched a new product on Monday."},
		{BlockID: "b-2", Text: "Acme Corp launched a new product on Monday morning."},
		{BlockID: "b-3", Text: "Totally unrelated content about widgets."},
	}
	// b-1 and b-2 are near-duplicates (cosine ~= 1); b-3 is distant.
	embeddings := [][]float32{
		unitVector(1.0),
		unitVector(1.0),
		unitVector(-1.0),
	}

	doc := &pipeline.Document{TenantID: "t-1", SourceID: "src-1", DocumentID: "d-1"}
	res, err := d.Apply(context.Background(), doc, blocks, embeddings)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("removed = %d; want 1", res.Removed)
	}
	if len(res.Blocks) != 2 {
		t.Fatalf("kept blocks = %d; want 2", len(res.Blocks))
	}
	// First-occurrence wins → b-1 + b-3 survive.
	ids := []string{res.Blocks[0].BlockID, res.Blocks[1].BlockID}
	if ids[0] != "b-1" || ids[1] != "b-3" {
		t.Fatalf("surviving blocks = %v; want [b-1 b-3]", ids)
	}
}

// TestPipelineDedup_DisabledByDefault_E2E sanity-checks the
// fail-open path: without the env var the deduplicator returns
// the input verbatim, even for byte-identical embeddings.
func TestPipelineDedup_DisabledByDefault_E2E(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_DEDUP_ENABLED", "")
	cfg := pipeline.LoadDedupConfigFromEnv()
	d := pipeline.NewDeduplicator(cfg)

	blocks := []pipeline.Block{
		{BlockID: "b-1", Text: "same"},
		{BlockID: "b-2", Text: "same"},
	}
	embeds := [][]float32{unitVector(1), unitVector(1)}

	res, err := d.Apply(context.Background(), &pipeline.Document{DocumentID: "d"}, blocks, embeds)
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if res.Removed != 0 {
		t.Fatalf("removed=%d; want 0 (dedup disabled)", res.Removed)
	}
	if len(res.Blocks) != 2 {
		t.Fatalf("kept=%d; want 2", len(res.Blocks))
	}
}
