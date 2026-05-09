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

	doclingv1 "github.com/kennguy3n/hunting-fishball/proto/docling/v1"
)

type fakeDoclingServer struct {
	doclingv1.UnimplementedDoclingServiceServer

	calls    atomic.Int32
	failTill int32
	failCode codes.Code
	resp     *doclingv1.ParseDocumentResponse
	delay    time.Duration
}

func (f *fakeDoclingServer) ParseDocument(ctx context.Context, _ *doclingv1.ParseDocumentRequest) (*doclingv1.ParseDocumentResponse, error) {
	n := f.calls.Add(1)
	if n <= f.failTill {
		return nil, status.Error(f.failCode, "synthetic failure")
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}

	return f.resp, nil
}

func newDoclingClient(t *testing.T, srv *fakeDoclingServer) doclingv1.DoclingServiceClient {
	t.Helper()

	lis := bufconn.Listen(1 << 16)
	gsrv := grpc.NewServer()
	doclingv1.RegisterDoclingServiceServer(gsrv, srv)
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

	return doclingv1.NewDoclingServiceClient(conn)
}

func TestParser_Parse_Happy(t *testing.T) {
	t.Parallel()

	srv := &fakeDoclingServer{
		resp: &doclingv1.ParseDocumentResponse{
			Blocks: []*doclingv1.ParsedBlock{
				{BlockId: "b1", Text: "hello", Type: doclingv1.BlockType_BLOCK_TYPE_PARAGRAPH},
				{BlockId: "b2", Text: "h2", Type: doclingv1.BlockType_BLOCK_TYPE_HEADING, HeadingLevel: 2},
			},
		},
	}
	client := newDoclingClient(t, srv)

	p, err := NewParser(ParseConfig{Client: client})
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	blocks, err := p.Parse(context.Background(), &Document{
		TenantID:     "t",
		DocumentID:   "d",
		Content:      []byte("body"),
		PrivacyLabel: "remote",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks: %d", len(blocks))
	}
	if blocks[0].Type != "paragraph" || blocks[1].Type != "heading" {
		t.Fatalf("types: %v", blocks)
	}
	if blocks[0].PrivacyLabel != "remote" {
		t.Fatalf("privacy_label not propagated: %q", blocks[0].PrivacyLabel)
	}
}

func TestParser_Parse_RetriesOnUnavailable(t *testing.T) {
	t.Parallel()

	srv := &fakeDoclingServer{
		failTill: 2,
		failCode: codes.Unavailable,
		resp:     &doclingv1.ParseDocumentResponse{},
	}
	client := newDoclingClient(t, srv)

	p, err := NewParser(ParseConfig{Client: client, MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	if _, err := p.Parse(context.Background(), &Document{TenantID: "t", DocumentID: "d", Content: []byte("x")}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := srv.calls.Load(); got != 3 {
		t.Fatalf("calls: %d, want 3", got)
	}
}

func TestParser_Parse_NoRetryOnInvalidArgument(t *testing.T) {
	t.Parallel()

	srv := &fakeDoclingServer{failTill: 10, failCode: codes.InvalidArgument}
	client := newDoclingClient(t, srv)

	p, _ := NewParser(ParseConfig{Client: client, MaxAttempts: 5, InitialBackoff: time.Microsecond})
	_, err := p.Parse(context.Background(), &Document{TenantID: "t", DocumentID: "d", Content: []byte("x")})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := srv.calls.Load(); got != 1 {
		t.Fatalf("expected 1 attempt (no retry), got %d", got)
	}
}

func TestParser_Parse_TimeoutFiresPerCall(t *testing.T) {
	t.Parallel()

	srv := &fakeDoclingServer{delay: 200 * time.Millisecond, resp: &doclingv1.ParseDocumentResponse{}}
	client := newDoclingClient(t, srv)

	p, _ := NewParser(ParseConfig{
		Client:         client,
		Timeout:        20 * time.Millisecond,
		MaxAttempts:    1,
		InitialBackoff: time.Microsecond,
	})
	_, err := p.Parse(context.Background(), &Document{TenantID: "t", DocumentID: "d", Content: []byte("x")})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestParser_NewParser_RejectsNilClient(t *testing.T) {
	t.Parallel()

	if _, err := NewParser(ParseConfig{}); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestParser_Parse_NilDoc(t *testing.T) {
	t.Parallel()

	srv := &fakeDoclingServer{resp: &doclingv1.ParseDocumentResponse{}}
	client := newDoclingClient(t, srv)

	p, _ := NewParser(ParseConfig{Client: client})
	if _, err := p.Parse(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestParser_Parse_RetryStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	srv := &fakeDoclingServer{failTill: 10, failCode: codes.Unavailable}
	client := newDoclingClient(t, srv)

	p, _ := NewParser(ParseConfig{Client: client, MaxAttempts: 5, InitialBackoff: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := p.Parse(ctx, &Document{TenantID: "t", DocumentID: "d", Content: []byte("x")})
	if err == nil {
		t.Fatal("expected error after ctx cancel")
	}
	if !errors.Is(err, context.DeadlineExceeded) && status.Code(err) == codes.OK {
		// Either a wrapped DeadlineExceeded or a propagated gRPC error
		// is acceptable; we just need the function to stop and return.
		t.Logf("err=%v (acceptable)", err)
	}
}
