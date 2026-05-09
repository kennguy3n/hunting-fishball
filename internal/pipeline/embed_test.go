package pipeline

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
)

type fakeEmbedServer struct {
	embeddingv1.UnimplementedEmbeddingServiceServer

	calls    atomic.Int32
	failTill int32
	failCode codes.Code
	dim      int
	model    string
	batches  atomic.Int32
}

func (f *fakeEmbedServer) ComputeEmbeddings(_ context.Context, req *embeddingv1.ComputeEmbeddingsRequest) (*embeddingv1.ComputeEmbeddingsResponse, error) {
	n := f.calls.Add(1)
	if n <= f.failTill {
		return nil, status.Error(f.failCode, "synthetic failure")
	}
	f.batches.Add(1)

	out := make([]*embeddingv1.Embedding, len(req.GetChunks()))
	for i := range req.GetChunks() {
		v := make([]float32, f.dim)
		for j := range v {
			v[j] = float32(i + j + 1)
		}
		out[i] = &embeddingv1.Embedding{Values: v}
	}

	model := f.model
	if model == "" {
		model = "test-model-v1"
	}

	return &embeddingv1.ComputeEmbeddingsResponse{Embeddings: out, ModelId: model, Dimensions: int32(f.dim)}, nil
}

func newEmbedClient(t *testing.T, srv *fakeEmbedServer) embeddingv1.EmbeddingServiceClient {
	t.Helper()

	lis := bufconn.Listen(1 << 16)
	gsrv := grpc.NewServer()
	embeddingv1.RegisterEmbeddingServiceServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(func() { gsrv.Stop() })

	dialer := func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough://bufconn",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return embeddingv1.NewEmbeddingServiceClient(conn)
}

func TestEmbedder_Embed_Happy(t *testing.T) {
	t.Parallel()

	srv := &fakeEmbedServer{dim: 4}
	client := newEmbedClient(t, srv)

	e, err := NewEmbedder(EmbedConfig{Local: client, BatchSize: 10})
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	out, model, err := e.EmbedTexts(context.Background(), "tenant-a", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len: %d", len(out))
	}
	if len(out[0]) != 4 {
		t.Fatalf("dim: %d", len(out[0]))
	}
	if model != "test-model-v1" {
		t.Fatalf("model: %q", model)
	}
}

func TestEmbedder_Embed_Batching(t *testing.T) {
	t.Parallel()

	srv := &fakeEmbedServer{dim: 2}
	client := newEmbedClient(t, srv)

	e, _ := NewEmbedder(EmbedConfig{Local: client, BatchSize: 2})
	out, _, err := e.EmbedTexts(context.Background(), "t", []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len: %d", len(out))
	}
	if got := srv.batches.Load(); got != 3 {
		t.Fatalf("batches: %d, want 3", got)
	}
}

type fakeRemoteEmbedder struct {
	called atomic.Int32
	err    error
	dim    int
}

func (f *fakeRemoteEmbedder) Embed(_ context.Context, _ string, chunks []string) ([][]float32, string, error) {
	f.called.Add(1)
	if f.err != nil {
		return nil, "", f.err
	}
	out := make([][]float32, len(chunks))
	for i := range chunks {
		v := make([]float32, f.dim)
		for j := range v {
			v[j] = -float32(i + j)
		}
		out[i] = v
	}

	return out, "remote-model", nil
}

func TestEmbedder_RemoteFallback(t *testing.T) {
	t.Parallel()

	local := &fakeEmbedServer{dim: 3}
	client := newEmbedClient(t, local)

	remote := &fakeRemoteEmbedder{dim: 3}
	e, _ := NewEmbedder(EmbedConfig{
		Local:       client,
		Remote:      remote,
		AllowRemote: func(tid string) bool { return tid == "tenant-remote" },
	})

	// Tenant on remote plan → remote called.
	if _, model, err := e.EmbedTexts(context.Background(), "tenant-remote", []string{"x"}); err != nil {
		t.Fatalf("EmbedTexts: %v", err)
	} else if model != "remote-model" {
		t.Fatalf("model: %q", model)
	}
	if got := remote.called.Load(); got != 1 {
		t.Fatalf("remote called: %d", got)
	}

	// Local-only tenant → local called.
	if _, _, err := e.EmbedTexts(context.Background(), "tenant-local", []string{"y"}); err != nil {
		t.Fatalf("EmbedTexts: %v", err)
	}
	if got := remote.called.Load(); got != 1 {
		t.Fatalf("remote called: %d, want still 1", got)
	}

	// Remote failure must fall back to local.
	remote.err = errors.New("boom")
	if _, _, err := e.EmbedTexts(context.Background(), "tenant-remote", []string{"z"}); err != nil {
		t.Fatalf("fallback EmbedTexts: %v", err)
	}
}

func TestEmbedder_RetryOnUnavailable(t *testing.T) {
	t.Parallel()

	srv := &fakeEmbedServer{failTill: 2, failCode: codes.Unavailable, dim: 2}
	client := newEmbedClient(t, srv)
	e, _ := NewEmbedder(EmbedConfig{Local: client, MaxAttempts: 3, InitialBackoff: time.Microsecond})

	if _, _, err := e.EmbedTexts(context.Background(), "t", []string{"a"}); err != nil {
		t.Fatalf("EmbedTexts: %v", err)
	}
	if got := srv.calls.Load(); got != 3 {
		t.Fatalf("calls: %d", got)
	}
}

func TestEmbedder_NewEmbedder_RejectsNilLocal(t *testing.T) {
	t.Parallel()

	if _, err := NewEmbedder(EmbedConfig{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestEmbedder_EmbedBlocks(t *testing.T) {
	t.Parallel()

	srv := &fakeEmbedServer{dim: 2}
	client := newEmbedClient(t, srv)
	e, _ := NewEmbedder(EmbedConfig{Local: client, BatchSize: 4})

	out, _, err := e.EmbedBlocks(context.Background(), "t", []Block{
		{Text: "a"}, {Text: "b"},
	})
	if err != nil {
		t.Fatalf("EmbedBlocks: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len: %d", len(out))
	}
}

func TestEmbedder_EmptyInput(t *testing.T) {
	t.Parallel()

	srv := &fakeEmbedServer{dim: 1}
	client := newEmbedClient(t, srv)
	e, _ := NewEmbedder(EmbedConfig{Local: client})

	out, _, err := e.EmbedTexts(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("EmbedTexts: %v", err)
	}
	if out != nil {
		t.Fatalf("want nil, got %v", out)
	}
	if got := srv.calls.Load(); got != 0 {
		t.Fatalf("expected no calls, got %d", got)
	}
}
