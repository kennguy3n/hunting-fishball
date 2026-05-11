package pipeline

// round8_wiring_test.go — unit tests for the four Round-8 wiring tasks
// (deduplicator → Stage 4, priority buffer → Submit, embedding-config
// resolver → Stage 3, retry analytics → runWithRetry).

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
)

// --------------------------------------------------------------------
// Task 1: Stage-4 storer applies the dedup pre-write hook.
// --------------------------------------------------------------------

func TestStorer_DedupWiring_DropsNearDuplicates(t *testing.T) {
	t.Parallel()
	v := &fakeVectorStore{}
	m := newFakeMetadataStore()
	dedup := NewDeduplicator(DedupConfig{Enabled: true, Threshold: 0.95, Connector: "test"})
	s, err := NewStorer(StoreConfig{Vector: v, Metadata: m, Connector: "test", Deduplicator: dedup})
	if err != nil {
		t.Fatalf("NewStorer: %v", err)
	}
	doc := &Document{TenantID: "t", DocumentID: "d", ContentHash: "h"}
	blocks := []Block{{BlockID: "b1", Text: "a"}, {BlockID: "b2", Text: "a"}}
	emb := [][]float32{{1, 0, 0}, {1, 0, 0}}
	if err := s.Store(context.Background(), doc, blocks, emb, ""); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if len(v.upserts) != 1 {
		t.Fatalf("dedup should have dropped duplicate vector; got %d upserts", len(v.upserts))
	}
	if len(m.chunks) != 1 {
		t.Fatalf("dedup should have dropped duplicate chunk row; got %d", len(m.chunks))
	}
}

func TestStorer_DedupDisabled_KeepsAll(t *testing.T) {
	t.Parallel()
	v := &fakeVectorStore{}
	m := newFakeMetadataStore()
	dedup := NewDeduplicator(DedupConfig{Enabled: false})
	s, err := NewStorer(StoreConfig{Vector: v, Metadata: m, Connector: "test", Deduplicator: dedup})
	if err != nil {
		t.Fatalf("NewStorer: %v", err)
	}
	doc := &Document{TenantID: "t", DocumentID: "d", ContentHash: "h"}
	blocks := []Block{{BlockID: "b1", Text: "a"}, {BlockID: "b2", Text: "a"}}
	emb := [][]float32{{1, 0, 0}, {1, 0, 0}}
	if err := s.Store(context.Background(), doc, blocks, emb, ""); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if len(v.upserts) != 2 {
		t.Fatalf("disabled dedup must keep all upserts; got %d", len(v.upserts))
	}
}

// --------------------------------------------------------------------
// Task 2: priority buffer fronts Stage 1.
// --------------------------------------------------------------------

func TestCoordinator_PriorityBufferOrdersHighBeforeLow(t *testing.T) {
	t.Parallel()

	type outcome struct {
		evt IngestEvent
	}
	var (
		mu      sync.Mutex
		ordered []outcome
	)
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h-" + evt.DocumentID}, nil
		}},
		fakeParse{fn: func(_ context.Context, doc *Document) ([]Block, error) {
			return []Block{{BlockID: "b", Text: doc.DocumentID}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)
	cfg.PriorityBuffer = NewPriorityBuffer(PriorityBufferConfig{
		HighCapacity:   8,
		NormalCapacity: 8,
		LowCapacity:    8,
		NormalAfter:    16,
		LowAfter:       16,
	})
	cfg.OnSuccess = func(_ context.Context, evt IngestEvent) {
		mu.Lock()
		ordered = append(ordered, outcome{evt: evt})
		mu.Unlock()
	}
	cfg.Workers = StageConfig{FetchWorkers: 1, ParseWorkers: 1, EmbedWorkers: 1, StoreWorkers: 1}
	cfg.QueueSize = 1

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	// Pre-stage four low-priority events before Run starts so the
	// priority buffer is fully populated when the drain goroutine
	// starts. Then push one high-priority event after Run starts;
	// the high-priority event should still be picked first because
	// Drain checks the high bucket before low.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 4; i++ {
		evt := IngestEvent{Kind: EventDocumentChanged, TenantID: "t", DocumentID: "low-" + string(rune('a'+i))}
		if err := cfg.PriorityBuffer.Push(ctx, evt, PriorityLow); err != nil {
			t.Fatalf("Push low: %v", err)
		}
	}
	highEvt := IngestEvent{Kind: EventReindex, TenantID: "t", DocumentID: "high-1"}
	if err := cfg.PriorityBuffer.Push(ctx, highEvt, PriorityHigh); err != nil {
		t.Fatalf("Push high: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Wait for all 5 events to drain.
	for {
		mu.Lock()
		n := len(ordered)
		mu.Unlock()
		if n == 5 {
			break
		}
		select {
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for events; got %d", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The high-priority event must have completed before all four
	// low-priority events. Find the index of "high-1" in ordered and
	// assert it is 0.
	if ordered[0].evt.DocumentID != "high-1" {
		t.Fatalf("expected high-1 to complete first; ordered=%v", ordered)
	}
}

func TestCoordinator_SubmitRoutesThroughPriorityBuffer(t *testing.T) {
	t.Parallel()
	var stored int32
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b", Text: "x"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)
	cfg.PriorityBuffer = NewPriorityBuffer(PriorityBufferConfig{})
	cfg.OnSuccess = func(_ context.Context, _ IngestEvent) { atomic.AddInt32(&stored, 1) }
	c, _ := NewCoordinator(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	for i := 0; i < 3; i++ {
		if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentChanged, TenantID: "t", DocumentID: "x"}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	for atomic.LoadInt32(&stored) < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&stored); got != 3 {
		t.Fatalf("OnSuccess: %d", got)
	}
}

// --------------------------------------------------------------------
// Task 3: embedding-config resolver picks per-source model.
// --------------------------------------------------------------------

type fakeEmbeddingConfigResolver struct {
	models map[string]string // key: tenantID|sourceID
}

func (f *fakeEmbeddingConfigResolver) ResolveEmbeddingModel(_ context.Context, tenantID, sourceID string) (string, int) {
	if m, ok := f.models[tenantID+"|"+sourceID]; ok {
		return m, 0
	}
	return "", 0
}

// stubEmbedClient captures the ModelID it receives so the per-source
// override path can be asserted without a full gRPC roundtrip.
type stubEmbedClient struct {
	mu          sync.Mutex
	lastModelID string
	returnModel string
}

func (s *stubEmbedClient) ComputeEmbeddings(_ context.Context, req *embeddingv1.ComputeEmbeddingsRequest, _ ...grpc.CallOption) (*embeddingv1.ComputeEmbeddingsResponse, error) {
	s.mu.Lock()
	s.lastModelID = req.GetModelId()
	s.mu.Unlock()
	out := make([]*embeddingv1.Embedding, len(req.GetChunks()))
	for i := range req.GetChunks() {
		out[i] = &embeddingv1.Embedding{Values: []float32{float32(i + 1)}}
	}
	model := s.returnModel
	if model == "" {
		model = req.GetModelId()
	}
	return &embeddingv1.ComputeEmbeddingsResponse{Embeddings: out, ModelId: model, Dimensions: 1}, nil
}

func TestEmbedder_PerSourceModelOverride(t *testing.T) {
	t.Parallel()
	// Build an Embedder via stubLocal that captures the ModelID it
	// receives, so we can assert the per-source override propagates.
	stub := &stubEmbedClient{returnModel: "remote-served"}
	resolver := &fakeEmbeddingConfigResolver{models: map[string]string{
		"t|s-override": "bge-large-v1",
	}}
	e, err := NewEmbedder(EmbedConfig{
		Local:          stub,
		ModelID:        "default-model",
		ConfigResolver: resolver,
	})
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}

	// Source with override: model_id sent to gRPC must be the override.
	_, _, err = e.EmbedBlocksForSource(context.Background(), "t", "s-override", []Block{{BlockID: "b", Text: "x"}})
	if err != nil {
		t.Fatalf("EmbedBlocksForSource: %v", err)
	}
	stub.mu.Lock()
	got := stub.lastModelID
	stub.mu.Unlock()
	if got != "bge-large-v1" {
		t.Fatalf("override not applied: got %q want bge-large-v1", got)
	}

	// Source without override: model_id sent to gRPC must be default.
	stub.mu.Lock()
	stub.lastModelID = ""
	stub.mu.Unlock()
	_, _, err = e.EmbedBlocksForSource(context.Background(), "t", "s-default", []Block{{BlockID: "b", Text: "x"}})
	if err != nil {
		t.Fatalf("EmbedBlocksForSource (default): %v", err)
	}
	stub.mu.Lock()
	got = stub.lastModelID
	stub.mu.Unlock()
	if got != "default-model" {
		t.Fatalf("default model not applied: got %q want default-model", got)
	}
}

// --------------------------------------------------------------------
// Task 4: retry analytics records outcomes for runWithRetry.
// --------------------------------------------------------------------

func TestCoordinator_RetryAnalyticsRecordsOutcomes(t *testing.T) {
	t.Parallel()
	ra := NewRetryAnalytics()
	var parseAttempts atomic.Int32
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, _ IngestEvent) (*Document, error) {
			return &Document{TenantID: "t", DocumentID: "d", ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			n := parseAttempts.Add(1)
			if n < 2 {
				return nil, errors.New("transient")
			}
			return []Block{{BlockID: "b", Text: "x"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)
	cfg.RetryAnalytics = ra
	c, _ := NewCoordinator(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentChanged, TenantID: "t", DocumentID: "d"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	stages := map[string]*StageRetryStats{}
	for _, st := range ra.Snapshot() {
		stages[st.Stage] = st
	}
	parse := stages["parse"]
	if parse == nil {
		t.Fatalf("expected parse stats; got %v", stages)
	}
	if parse.Retries < 1 {
		t.Fatalf("expected at least one retry for parse; got %+v", parse)
	}
	if parse.Successes < 1 {
		t.Fatalf("expected at least one success for parse; got %+v", parse)
	}
	if parse.SuccessAfterRtry < 1 {
		t.Fatalf("expected success_after_retry > 0; got %+v", parse)
	}
}

// --------------------------------------------------------------------
// Helpers.
// --------------------------------------------------------------------

// Compile-time check: *audit.Repository satisfies DedupAuditSink (used
// by the dedup wiring in cmd/ingest/main.go).
var _ DedupAuditSink = (*audit.Repository)(nil)
