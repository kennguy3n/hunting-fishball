//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
)

// TestEmbedding_ComputeReturnsNonZeroVectors sends a small batch
// through the embedding service and verifies (a) the right number
// of vectors come back, (b) every vector has the same non-zero
// dimension and (c) the dimensions field is populated.
func TestEmbedding_ComputeReturnsNonZeroVectors(t *testing.T) {
	t.Parallel()

	target := envOr("EMBEDDING_TARGET", "localhost:50052")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	c := embeddingv1.NewEmbeddingServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	chunks := []string{
		"a quick brown fox",
		"jumps over the lazy dog",
		"context engines are cool",
	}
	resp, err := c.ComputeEmbeddings(ctx, &embeddingv1.ComputeEmbeddingsRequest{
		TenantId: "tenant-it",
		Chunks:   chunks,
	})
	if err != nil {
		t.Fatalf("ComputeEmbeddings: %v", err)
	}
	if len(resp.GetEmbeddings()) != len(chunks) {
		t.Fatalf("got %d embeddings, want %d", len(resp.GetEmbeddings()), len(chunks))
	}
	if resp.GetDimensions() <= 0 {
		t.Fatalf("dimensions: %d", resp.GetDimensions())
	}
	dim := int(resp.GetDimensions())
	for i, e := range resp.GetEmbeddings() {
		if len(e.GetValues()) != dim {
			t.Fatalf("embedding %d has %d values, want %d", i, len(e.GetValues()), dim)
		}
		// At least one component must be non-zero — otherwise the
		// model is broken.
		hasNonZero := false
		for _, v := range e.GetValues() {
			if v != 0 {
				hasNonZero = true
				break
			}
		}
		if !hasNonZero {
			t.Fatalf("embedding %d is all zeros", i)
		}
	}
}

// TestEmbedding_RejectsMissingTenant verifies the tenant_id contract
// is enforced server-side.
func TestEmbedding_RejectsMissingTenant(t *testing.T) {
	t.Parallel()

	target := envOr("EMBEDDING_TARGET", "localhost:50052")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	c := embeddingv1.NewEmbeddingServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = c.ComputeEmbeddings(ctx, &embeddingv1.ComputeEmbeddingsRequest{
		Chunks: []string{"x"},
	})
	if err == nil {
		t.Fatalf("expected error for missing tenant_id")
	}
}
