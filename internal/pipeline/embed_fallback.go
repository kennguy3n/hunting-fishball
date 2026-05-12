// embed_fallback.go — Round-13 Task 19.
//
// When the gRPC embedding sidecar's circuit breaker opens, the
// pipeline can either fail Stage 3 (current behaviour) or fall
// back to an in-process Go-native embedder so the document still
// flows through with `degraded_embedding: true` recorded on the
// stored chunk.
//
// The fallback is intentionally lightweight: a fixed-width
// hashing trick that emits a deterministic float32 vector of
// FallbackDim dimensions per chunk. It is NOT a quality
// substitute for real embeddings — the operator-facing flag
// `degraded_embedding: true` lets the retrieval side
// down-weight / filter chunks produced this way, and a periodic
// reindex job is expected to replace them once the sidecar
// recovers.
//
// Gated on CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED. When the env
// var is unset the embedder returns ErrFallbackDisabled and the
// caller treats Stage 3 as failed (unchanged behaviour).
package pipeline

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// FallbackDim is the fixed dimensionality of fallback vectors.
const FallbackDim = 256

// FallbackModelID is the model identifier recorded on chunks
// embedded by the fallback path. The retrieval side checks this
// string to decide whether to include the chunk.
const FallbackModelID = "hf-fallback-v1"

// ErrFallbackDisabled is returned by FallbackEmbedder.Embed when
// CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED is unset / "false".
var ErrFallbackDisabled = errors.New("pipeline: embedding fallback disabled")

// FallbackEmbedder produces deterministic FallbackDim-vectors
// using a TF-IDF-like hashing trick (no IDF table — the trick is
// pure hashing for determinism + zero state).
type FallbackEmbedder struct {
	enabled bool
}

// NewFallbackEmbedder reads the env gate and returns the
// embedder. The returned embedder is always non-nil so callers
// can call Embed and switch on ErrFallbackDisabled.
func NewFallbackEmbedder() *FallbackEmbedder {
	return &FallbackEmbedder{enabled: fallbackEnabled()}
}

func fallbackEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXT_ENGINE_EMBED_FALLBACK_ENABLED")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// Enabled reports whether the env gate is on. Exported for
// tests and for the pipeline coordinator to log a one-time info
// line at startup.
func (f *FallbackEmbedder) Enabled() bool { return f != nil && f.enabled }

// Embed computes a degraded-mode vector per chunk. Returns
// ErrFallbackDisabled when the env gate is off.
func (f *FallbackEmbedder) Embed(_ context.Context, _ string, chunks []string) ([][]float32, string, error) {
	if f == nil || !f.enabled {
		return nil, "", ErrFallbackDisabled
	}
	return f.EmbedWithReason(context.Background(), chunks, FallbackReasonUnknown)
}

// FallbackReason names the reason the caller fell back to the
// in-process embedder. Used as the value of the `reason` label
// on the Round-14 fallback counter so dashboards can split fast
// path failures (gRPC down, timeout, breaker open).
type FallbackReason string

const (
	FallbackReasonUnknown      FallbackReason = "unknown"
	FallbackReasonGRPCDown     FallbackReason = "grpc_down"
	FallbackReasonTimeout      FallbackReason = "timeout"
	FallbackReasonCircuitOpen  FallbackReason = "circuit_open"
	FallbackReasonExplicitFlag FallbackReason = "explicit_flag"
)

// EmbedWithReason is the metric-aware entry point. The
// coordinator-side wiring (added in a follow-up task) labels the
// trigger reason so the operator can see why fallback fired in
// production. The legacy Embed wrapper keeps existing callers
// working by attributing the trigger to "unknown".
func (f *FallbackEmbedder) EmbedWithReason(_ context.Context, chunks []string, reason FallbackReason) ([][]float32, string, error) {
	if f == nil || !f.enabled {
		return nil, "", ErrFallbackDisabled
	}
	if reason == "" {
		reason = FallbackReasonUnknown
	}
	start := time.Now()
	out := make([][]float32, len(chunks))
	for i, text := range chunks {
		out[i] = hashEmbed(text)
	}
	observability.EmbeddingFallbackByReason.WithLabelValues(string(reason)).Inc()
	observability.EmbeddingFallbackLatency.Observe(time.Since(start).Seconds())
	return out, FallbackModelID, nil
}

// hashEmbed implements the hashing trick. Each token contributes
// +1 to the bucket dim = hash(token) % FallbackDim; the resulting
// vector is L2-normalised so cosine similarity stays meaningful.
func hashEmbed(text string) []float32 {
	vec := make([]float32, FallbackDim)
	tokens := tokenize(text)
	for _, tok := range tokens {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		vec[int(h.Sum32())%FallbackDim] += 1
	}
	// L2 normalise.
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if sumSq == 0 {
		return vec
	}
	norm := float32(1.0 / math.Sqrt(sumSq))
	for i := range vec {
		vec[i] *= norm
	}
	return vec
}

func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ToLower(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	return fields
}
