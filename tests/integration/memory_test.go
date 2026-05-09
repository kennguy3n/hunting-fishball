//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	memoryv1 "github.com/kennguy3n/hunting-fishball/proto/memory/v1"
)

// TestMemory_WriteThenSearch writes a memory and verifies it can be
// recalled within the same tenant + user. Mem0 takes a moment to
// commit asynchronously to its vector store, so we retry the search
// for up to ~10s before giving up.
func TestMemory_WriteThenSearch(t *testing.T) {
	t.Parallel()

	target := envOr("MEMORY_TARGET", "localhost:50053")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	c := memoryv1.NewMemoryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	wresp, err := c.WriteMemory(ctx, &memoryv1.WriteMemoryRequest{
		TenantId:  "tenant-it",
		UserId:    "user-it",
		SessionId: "session-it",
		Content:   "the user prefers a dark theme",
	})
	if err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	if wresp.GetId() == "" {
		t.Fatalf("WriteMemory returned empty id")
	}
	if wresp.GetCreatedAt() <= 0 {
		t.Fatalf("WriteMemory.created_at: %d", wresp.GetCreatedAt())
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		sresp, serr := c.SearchMemory(ctx, &memoryv1.SearchMemoryRequest{
			TenantId:  "tenant-it",
			UserId:    "user-it",
			SessionId: "session-it",
			Query:     "dark theme",
			TopK:      5,
		})
		if serr == nil && len(sresp.GetResults()) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SearchMemory: did not recall written memory in time (last err=%v)", serr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestMemory_RejectsMissingTenant verifies the tenant scope contract
// is enforced.
func TestMemory_RejectsMissingTenant(t *testing.T) {
	t.Parallel()

	target := envOr("MEMORY_TARGET", "localhost:50053")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	c := memoryv1.NewMemoryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = c.WriteMemory(ctx, &memoryv1.WriteMemoryRequest{
		Content: "x",
	})
	if err == nil {
		t.Fatalf("expected error for missing tenant_id")
	}
}
