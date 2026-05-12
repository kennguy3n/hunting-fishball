package retrieval_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	rerankerv1 "github.com/kennguy3n/hunting-fishball/proto/reranker/v1"
)

// fakeRerankerServer is a minimal in-process gRPC reranker stub.
type fakeRerankerServer struct {
	rerankerv1.UnimplementedRerankerServiceServer

	scoreByID map[string]float32
	err       error
	last      *rerankerv1.RerankRequest
}

func (f *fakeRerankerServer) Rerank(_ context.Context, req *rerankerv1.RerankRequest) (*rerankerv1.RerankResponse, error) {
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*rerankerv1.ScoredCandidate, 0, len(req.GetCandidates()))
	for _, c := range req.GetCandidates() {
		s, ok := f.scoreByID[c.GetChunkId()]
		if !ok {
			s = 0
		}
		out = append(out, &rerankerv1.ScoredCandidate{
			ChunkId: c.GetChunkId(),
			Score:   s,
		})
	}

	return &rerankerv1.RerankResponse{Scored: out, ModelId: "fake-model"}, nil
}

func newCrossEncoderClient(t *testing.T, srv *fakeRerankerServer) rerankerv1.RerankerServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 10)
	s := grpc.NewServer()
	rerankerv1.RegisterRerankerServiceServer(s, srv)
	t.Cleanup(s.Stop)
	go func() { _ = s.Serve(lis) }()
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return rerankerv1.NewRerankerServiceClient(conn)
}

func TestCrossEncoderReranker_ReordersByGRPCScore(t *testing.T) {
	t.Parallel()
	srv := &fakeRerankerServer{
		scoreByID: map[string]float32{"a": 0.1, "b": 0.9, "c": 0.5},
	}
	client := newCrossEncoderClient(t, srv)
	rer := retrieval.NewCrossEncoderReranker(retrieval.CrossEncoderConfig{
		Client:  client,
		Timeout: 2 * time.Second,
		ModelID: "test-model",
	})
	matches := []*retrieval.Match{
		{ID: "a", Text: "alpha", Score: 0.3},
		{ID: "b", Text: "bravo", Score: 0.2},
		{ID: "c", Text: "charlie", Score: 0.1},
	}
	out, err := rer.Rerank(retrieval.WithTenantContext(context.Background(), "t1"), "query", matches)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if got := []string{out[0].ID, out[1].ID, out[2].ID}; got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Fatalf("unexpected order: %v", got)
	}
	if out[0].Score != 0.9 {
		t.Fatalf("expected top score = 0.9, got %v", out[0].Score)
	}
	if out[0].OriginalScore != 0.2 {
		t.Fatalf("expected OriginalScore to preserve pre-rerank score, got %v", out[0].OriginalScore)
	}
	if srv.last.GetTenantId() != "t1" || srv.last.GetModelId() != "test-model" {
		t.Fatalf("unexpected request envelope: %+v", srv.last)
	}
}

func TestCrossEncoderReranker_FallbackOnRPCError(t *testing.T) {
	t.Parallel()
	srv := &fakeRerankerServer{err: errors.New("sidecar down")}
	client := newCrossEncoderClient(t, srv)
	called := 0
	fallback := &recordingReranker{name: "fb", onRerank: func() { called++ }}
	rer := retrieval.NewCrossEncoderReranker(retrieval.CrossEncoderConfig{
		Client:   client,
		Fallback: fallback,
	})
	matches := []*retrieval.Match{
		{ID: "a", Text: "alpha", Score: 0.1},
	}
	out, err := rer.Rerank(context.Background(), "q", matches)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected fallback to be invoked exactly once, got %d", called)
	}
	if len(out) != 1 || out[0].ID != "a" {
		t.Fatalf("expected passthrough on fallback, got %+v", out)
	}
}

func TestCrossEncoderReranker_NilClientUsesFallback(t *testing.T) {
	t.Parallel()
	called := 0
	fallback := &recordingReranker{name: "fb", onRerank: func() { called++ }}
	rer := retrieval.NewCrossEncoderReranker(retrieval.CrossEncoderConfig{
		Client:   nil,
		Fallback: fallback,
	})
	matches := []*retrieval.Match{{ID: "a", Text: "alpha", Score: 0.1}}
	if _, err := rer.Rerank(context.Background(), "q", matches); err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected fallback once when client is nil, got %d", called)
	}
}

func TestCrossEncoderReranker_TruncatesAtMaxCandidates(t *testing.T) {
	t.Parallel()
	srv := &fakeRerankerServer{
		scoreByID: map[string]float32{"a": 0.9, "b": 0.8},
	}
	client := newCrossEncoderClient(t, srv)
	rer := retrieval.NewCrossEncoderReranker(retrieval.CrossEncoderConfig{
		Client:        client,
		MaxCandidates: 2,
	})
	matches := []*retrieval.Match{
		{ID: "a", Text: "alpha", Score: 0.5},
		{ID: "b", Text: "bravo", Score: 0.5},
		{ID: "c", Text: "charlie", Score: 0.5}, // outside MaxCandidates
	}
	out, err := rer.Rerank(context.Background(), "q", matches)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(srv.last.GetCandidates()) != 2 {
		t.Fatalf("expected exactly 2 candidates forwarded, got %d", len(srv.last.GetCandidates()))
	}
	// The truncated candidate "c" must still be present in the
	// output (just not reranked).
	ids := map[string]bool{}
	for _, m := range out {
		ids[m.ID] = true
	}
	if !ids["c"] {
		t.Fatalf("truncated tail dropped from output: %+v", out)
	}
}

func TestCrossEncoderReranker_TopKBoundsOutput(t *testing.T) {
	t.Parallel()
	srv := &fakeRerankerServer{
		scoreByID: map[string]float32{"a": 0.1, "b": 0.9},
	}
	client := newCrossEncoderClient(t, srv)
	rer := retrieval.NewCrossEncoderReranker(retrieval.CrossEncoderConfig{
		Client: client,
		TopK:   1,
	})
	matches := []*retrieval.Match{
		{ID: "a", Text: "alpha", Score: 0.1},
		{ID: "b", Text: "bravo", Score: 0.2},
	}
	out, err := rer.Rerank(context.Background(), "q", matches)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(out) != 1 || out[0].ID != "b" {
		t.Fatalf("expected top-1 to be b, got %+v", out)
	}
}

func TestCrossEncoderEnabled_RespectsEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
		{"yes", true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CONTEXT_ENGINE_CROSS_ENCODER_ENABLED", tc.val)
			if got := retrieval.CrossEncoderEnabled(); got != tc.want {
				t.Fatalf("CrossEncoderEnabled(%q): got %v want %v", tc.val, got, tc.want)
			}
		})
	}
}

// recordingReranker is a tiny Reranker that bumps a counter on each
// call. Used by the fallback tests.
type recordingReranker struct {
	name     string
	onRerank func()
}

func (r *recordingReranker) Rerank(_ context.Context, _ string, matches []*retrieval.Match) ([]*retrieval.Match, error) {
	if r.onRerank != nil {
		r.onRerank()
	}

	return matches, nil
}
