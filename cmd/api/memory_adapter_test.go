package main

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	memoryv1 "github.com/kennguy3n/hunting-fishball/proto/memory/v1"
)

// fakeMemoryServer captures every SearchMemoryRequest so the test
// can assert tenant_id is populated by the Go memoryAdapter.
type fakeMemoryServer struct {
	memoryv1.UnimplementedMemoryServiceServer
	lastReq *memoryv1.SearchMemoryRequest
}

func (f *fakeMemoryServer) SearchMemory(_ context.Context, req *memoryv1.SearchMemoryRequest) (*memoryv1.SearchMemoryResponse, error) {
	f.lastReq = req
	return &memoryv1.SearchMemoryResponse{}, nil
}

// TestMemoryAdapter_PassesTenantID is the Go side of the Phase 8
// Task 17 partitioning verification: the adapter must pass the
// caller's tenant_id on every gRPC call so the Python server can
// scope by tenant prefix.
func TestMemoryAdapter_PassesTenantID(t *testing.T) {
	t.Parallel()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	fake := &fakeMemoryServer{}
	memoryv1.RegisterMemoryServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(
		"passthrough:bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	adapter := &memoryAdapter{c: memoryv1.NewMemoryServiceClient(conn)}
	if _, err := adapter.Search(context.Background(), "tenant-xyz", "hello", 5); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if fake.lastReq == nil {
		t.Fatalf("server got no request")
	}
	if got := fake.lastReq.GetTenantId(); got != "tenant-xyz" {
		t.Fatalf("tenant_id = %q, want %q", got, "tenant-xyz")
	}
	if got := fake.lastReq.GetTopK(); got != 5 {
		t.Fatalf("top_k = %d, want 5", got)
	}
	if got := fake.lastReq.GetQuery(); got != "hello" {
		t.Fatalf("query = %q, want %q", got, "hello")
	}
}
