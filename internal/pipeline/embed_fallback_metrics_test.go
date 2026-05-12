package pipeline_test

// embed_fallback_metrics_test.go — Round-14 Task 13.
//
// Asserts that EmbedWithReason increments the new reason-labeled
// counter and observes the fallback-latency histogram.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestFallbackEmbedder_EmbedWithReason_IncrementsCounter(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED", "1")
	observability.ResetForTest()
	emb := pipeline.NewFallbackEmbedder()
	if !emb.Enabled() {
		t.Fatal("expected fallback enabled")
	}
	_, _, err := emb.EmbedWithReason(context.Background(), []string{"alpha"}, pipeline.FallbackReasonCircuitOpen)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	got := testutil.ToFloat64(observability.EmbeddingFallbackByReason.WithLabelValues("circuit_open"))
	if got != 1 {
		t.Fatalf("counter for circuit_open = %f, want 1", got)
	}
	histCount := testutil.CollectAndCount(observability.EmbeddingFallbackLatency)
	if histCount == 0 {
		t.Fatal("expected at least one fallback latency sample")
	}
}

func TestFallbackEmbedder_EmbedWithReason_DefaultsUnknown(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED", "1")
	observability.ResetForTest()
	emb := pipeline.NewFallbackEmbedder()
	if _, _, err := emb.EmbedWithReason(context.Background(), []string{"alpha"}, ""); err != nil {
		t.Fatalf("embed: %v", err)
	}
	got := testutil.ToFloat64(observability.EmbeddingFallbackByReason.WithLabelValues("unknown"))
	if got != 1 {
		t.Fatalf("counter for unknown = %f, want 1", got)
	}
}
