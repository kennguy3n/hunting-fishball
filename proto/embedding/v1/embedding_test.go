package embeddingv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestComputeEmbeddings_RoundTrip(t *testing.T) {
	t.Parallel()

	req := &ComputeEmbeddingsRequest{
		TenantId: "tenant-a",
		Chunks:   []string{"hello", "world"},
		ModelId:  "bge-base",
	}
	resp := &ComputeEmbeddingsResponse{
		Embeddings: []*Embedding{
			{Values: []float32{0.1, 0.2, 0.3}},
			{Values: []float32{0.4, 0.5, 0.6}},
		},
		ModelId:    "bge-base",
		Dimensions: 3,
	}

	for _, m := range []proto.Message{req, resp} {
		wire, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		clone := proto.Clone(m)
		// Reset to ensure Unmarshal repopulates everything.
		proto.Reset(clone)
		if err := proto.Unmarshal(wire, clone); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !proto.Equal(m, clone) {
			t.Fatalf("round-trip mismatch: %+v vs %+v", m, clone)
		}
	}
}
