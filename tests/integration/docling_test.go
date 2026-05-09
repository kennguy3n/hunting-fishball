//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	doclingv1 "github.com/kennguy3n/hunting-fishball/proto/docling/v1"
)

// TestDocling_ParseSampleHTML sends a tiny HTML string to the Docling
// service and asserts the server responds with at least one block
// and a non-zero structure. We use HTML rather than a real PDF so
// the test stays under a second even on a cold container.
func TestDocling_ParseSampleHTML(t *testing.T) {
	t.Parallel()

	target := envOr("DOCLING_TARGET", "localhost:50051")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	c := doclingv1.NewDoclingServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	html := []byte("<html><body><h1>Phase 3 Smoke</h1><p>hello world</p></body></html>")
	resp, err := c.ParseDocument(ctx, &doclingv1.ParseDocumentRequest{
		TenantId:    "tenant-it",
		DocumentId:  "doc-it",
		Content:     html,
		ContentType: "text/html",
	})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if len(resp.GetBlocks()) == 0 {
		t.Fatalf("expected at least one parsed block")
	}
	hasText := false
	for _, b := range resp.GetBlocks() {
		if b.GetText() != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		t.Fatalf("no parsed block had non-empty text")
	}
}

// TestDocling_RejectsMissingTenant verifies the contract that the
// server refuses unscoped requests. The Python side aborts with
// INVALID_ARGUMENT, which the Go gRPC stub surfaces as a non-nil
// error.
func TestDocling_RejectsMissingTenant(t *testing.T) {
	t.Parallel()

	target := envOr("DOCLING_TARGET", "localhost:50051")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	c := doclingv1.NewDoclingServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = c.ParseDocument(ctx, &doclingv1.ParseDocumentRequest{
		Content:     []byte("<html></html>"),
		ContentType: "text/html",
	})
	if err == nil {
		t.Fatalf("expected error for missing tenant_id")
	}
}
