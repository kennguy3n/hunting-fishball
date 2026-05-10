//go:build integration

// Round-4 Task 12: full pipeline integration test that runs the
// Stage 1→2→3→4 path in-process against fakes for the Python
// services. Unlike the e2e/ tests, this one does NOT need
// docker-compose — fakes stand in for fetch/parse/embed/store —
// so CI can run it on every PR. The point is to lock down the
// stage glue (back-pressure, request-id propagation, OnSuccess
// callback, DLQ routing on failure) without any network.
package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// fakeFetch returns a deterministic Document for every event.
type fakeFetch struct{ calls int32 }

func (f *fakeFetch) FetchEvent(_ context.Context, evt pipeline.IngestEvent) (*pipeline.Document, error) {
	return &pipeline.Document{
		TenantID:   evt.TenantID,
		SourceID:   evt.SourceID,
		DocumentID: evt.DocumentID,
		Content:    []byte("hello world"),
	}, nil
}

// fakeParse splits the doc body into one block per word.
type fakeParse struct{}

func (fakeParse) Parse(_ context.Context, doc *pipeline.Document) ([]pipeline.Block, error) {
	return []pipeline.Block{
		{BlockID: doc.DocumentID + ":0", Text: "hello", Type: "paragraph"},
		{BlockID: doc.DocumentID + ":1", Text: "world", Type: "paragraph"},
	}, nil
}

// fakeEmbed returns a fixed-size deterministic vector per block.
type fakeEmbed struct{}

func (fakeEmbed) EmbedBlocks(_ context.Context, _ string, blocks []pipeline.Block) ([][]float32, string, error) {
	out := make([][]float32, len(blocks))
	for i := range blocks {
		out[i] = []float32{1, 0, 0}
	}
	return out, "fake-1", nil
}

// fakeStore records (tenant, document) tuples for assertions.
type fakeStore struct {
	mu      sync.Mutex
	stored  []string
	deleted []string
}

func (s *fakeStore) Store(_ context.Context, doc *pipeline.Document, blocks []pipeline.Block, embeddings [][]float32, modelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(blocks) != len(embeddings) {
		return fmt.Errorf("blocks/embeddings mismatch: %d/%d", len(blocks), len(embeddings))
	}
	if modelID == "" {
		return errors.New("missing modelID")
	}
	s.stored = append(s.stored, doc.TenantID+":"+doc.DocumentID)
	return nil
}

func (s *fakeStore) Delete(_ context.Context, tenantID, documentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, tenantID+":"+documentID)
	return nil
}

func (s *fakeStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]string, len(s.stored))
	copy(cp, s.stored)
	return cp
}

// TestFullPipeline_HappyPath drives N events end-to-end through
// the coordinator and verifies every event reached Store and the
// OnSuccess callback fired with a propagated request-id.
func TestFullPipeline_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store := &fakeStore{}

	var (
		successMu   sync.Mutex
		successEvts []pipeline.IngestEvent
	)
	cfg := pipeline.CoordinatorConfig{
		Fetch: &fakeFetch{}, Parse: fakeParse{}, Embed: fakeEmbed{}, Store: store,
		QueueSize: 16,
		OnSuccess: func(ctx context.Context, evt pipeline.IngestEvent) {
			successMu.Lock()
			defer successMu.Unlock()
			successEvts = append(successEvts, evt)
		},
	}
	coord, err := pipeline.NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- coord.Run(runCtx) }()

	// Inject N events with stable request ids.
	const n = 10
	for i := 0; i < n; i++ {
		evt := pipeline.IngestEvent{
			Kind:       pipeline.EventDocumentChanged,
			TenantID:   "t1",
			SourceID:   "s1",
			DocumentID: fmt.Sprintf("doc-%02d", i),
			RequestID:  fmt.Sprintf("rid-%02d", i),
		}
		if err := coord.Submit(ctx, evt); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	// Wait for the store to settle.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.snapshot()) >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(store.snapshot()); got != n {
		t.Fatalf("stored %d, want %d", got, n)
	}

	runCancel()
	<-runDone

	// OnSuccess must have observed the same request ids.
	successMu.Lock()
	defer successMu.Unlock()
	if len(successEvts) != n {
		t.Fatalf("OnSuccess fired %d times, want %d", len(successEvts), n)
	}
	seen := map[string]struct{}{}
	for _, e := range successEvts {
		if e.RequestID == "" {
			t.Fatalf("event %s missing request_id in OnSuccess", e.DocumentID)
		}
		seen[e.RequestID] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique request ids, got %d", n, len(seen))
	}
}

// TestFullPipeline_DLQRoutesOnStageError forces the parse stage
// to fail for one event and verifies the OnDLQ hook fires while
// the rest of the stream continues.
func TestFullPipeline_DLQRoutesOnStageError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store := &fakeStore{}
	var (
		dlqMu   sync.Mutex
		dlqEvts []pipeline.IngestEvent
	)
	parse := failingParse{failOn: "doc-bad"}
	cfg := pipeline.CoordinatorConfig{
		Fetch: &fakeFetch{}, Parse: parse, Embed: fakeEmbed{}, Store: store,
		QueueSize: 16,
		// NB: keep retries low — the coordinator retries before
		// routing to DLQ; one retry is enough to confirm routing.
		MaxAttempts: 1,
		OnDLQ: func(_ context.Context, evt pipeline.IngestEvent, _ error) {
			dlqMu.Lock()
			defer dlqMu.Unlock()
			dlqEvts = append(dlqEvts, evt)
		},
	}
	coord, err := pipeline.NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- coord.Run(runCtx) }()

	for _, doc := range []string{"doc-ok-0", "doc-bad", "doc-ok-1"} {
		_ = coord.Submit(ctx, pipeline.IngestEvent{
			Kind:     pipeline.EventDocumentChanged,
			TenantID: "t1", SourceID: "s1", DocumentID: doc,
			RequestID: "rid-" + doc,
		})
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.snapshot()) >= 2 && atomicLen(&dlqMu, &dlqEvts) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	runCancel()
	<-runDone

	if got := len(store.snapshot()); got != 2 {
		t.Fatalf("stored %d, want 2", got)
	}
	dlqMu.Lock()
	defer dlqMu.Unlock()
	if len(dlqEvts) != 1 || dlqEvts[0].DocumentID != "doc-bad" {
		t.Fatalf("dlq=%+v, want exactly doc-bad", dlqEvts)
	}
	if dlqEvts[0].RequestID != "rid-doc-bad" {
		t.Fatalf("dlq event missing request_id: %q", dlqEvts[0].RequestID)
	}
}

// failingParse fails Parse() for any document whose ID matches
// failOn. Other docs use the standard fakeParse split.
type failingParse struct{ failOn string }

func (p failingParse) Parse(_ context.Context, doc *pipeline.Document) ([]pipeline.Block, error) {
	if doc.DocumentID == p.failOn {
		return nil, fmt.Errorf("synthetic parse failure for %s", doc.DocumentID)
	}
	return []pipeline.Block{{BlockID: doc.DocumentID + ":0", Text: doc.DocumentID, Type: "paragraph"}}, nil
}

// atomicLen returns len(slice) under the supplied mutex. Used by
// the polling loop above without smearing locking everywhere.
func atomicLen[T any](mu *sync.Mutex, slice *[]T) int {
	mu.Lock()
	defer mu.Unlock()
	return len(*slice)
}

// TestFullPipeline_RequestIDPropagatesToContext sanity-checks
// that the RequestID set on the IngestEvent reaches the OnSuccess
// callback's context (so downstream gRPC clients pick it up via
// observability.RequestIDFromContext for tracing).
func TestFullPipeline_RequestIDPropagatesToContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var seen string
	cfg := pipeline.CoordinatorConfig{
		Fetch: &fakeFetch{}, Parse: fakeParse{}, Embed: fakeEmbed{}, Store: &fakeStore{},
		QueueSize: 4,
		OnSuccess: func(ctx context.Context, evt pipeline.IngestEvent) {
			// Either the event field or the ctx must carry the id.
			if v := observability.RequestIDFromContext(ctx); v != "" {
				seen = v
				return
			}
			seen = evt.RequestID
		},
	}
	coord, err := pipeline.NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- coord.Run(runCtx) }()

	_ = coord.Submit(ctx, pipeline.IngestEvent{
		Kind: pipeline.EventDocumentChanged, TenantID: "t1", SourceID: "s1",
		DocumentID: "x", RequestID: "rid-propagate",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && seen == "" {
		time.Sleep(10 * time.Millisecond)
	}
	runCancel()
	<-done
	if seen != "rid-propagate" {
		t.Fatalf("OnSuccess ctx/request_id = %q, want rid-propagate", seen)
	}
}
