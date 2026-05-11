package pipeline_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestFallbackEmbedder_DisabledByDefault(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED", "")
	emb := pipeline.NewFallbackEmbedder()
	_, _, err := emb.Embed(context.Background(), "t-1", []string{"hello world"})
	if !errors.Is(err, pipeline.ErrFallbackDisabled) {
		t.Fatalf("expected ErrFallbackDisabled; got %v", err)
	}
	if emb.Enabled() {
		t.Fatal("expected Enabled()=false when env unset")
	}
}

func TestFallbackEmbedder_ProducesDeterministicVector(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED", "true")
	emb := pipeline.NewFallbackEmbedder()
	a, modelA, err := emb.Embed(context.Background(), "t-1", []string{"hello world"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	b, modelB, err := emb.Embed(context.Background(), "t-1", []string{"hello world"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if modelA != pipeline.FallbackModelID || modelB != pipeline.FallbackModelID {
		t.Fatalf("model id wrong: %s vs %s", modelA, modelB)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("wrong count: %d/%d", len(a), len(b))
	}
	if len(a[0]) != pipeline.FallbackDim || len(b[0]) != pipeline.FallbackDim {
		t.Fatalf("dim wrong: %d/%d", len(a[0]), len(b[0]))
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("non-deterministic at index %d: %v vs %v", i, a[0][i], b[0][i])
		}
	}
}

func TestFallbackEmbedder_L2Normalised(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED", "1")
	emb := pipeline.NewFallbackEmbedder()
	out, _, err := emb.Embed(context.Background(), "t-1", []string{"alpha beta gamma delta epsilon"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	var sumSq float64
	for _, v := range out[0] {
		sumSq += float64(v) * float64(v)
	}
	if math.Abs(math.Sqrt(sumSq)-1) > 1e-4 {
		t.Fatalf("vector not L2-normalised; |v|=%f", math.Sqrt(sumSq))
	}
}

func TestFallbackEmbedder_DifferentTextsDifferentVectors(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED", "true")
	emb := pipeline.NewFallbackEmbedder()
	out, _, err := emb.Embed(context.Background(), "t-1", []string{"alpha beta", "gamma delta"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	same := true
	for i := range out[0] {
		if out[0][i] != out[1][i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("distinct texts produced identical fallback vectors")
	}
}
