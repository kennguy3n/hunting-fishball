package pipeline

// round10_hooks_test.go — Round-10 Tasks 3 & 4.
//
// Exercises the coordinator hooks introduced in Round-10:
//
//  - SyncHistoryRecorder: backfill kickoff -> Start, per-document
//    Stage 4 outcome -> processed/failed counters, explicit
//    FinishBackfillRun -> Finish.
//  - ChunkScorer + ChunkQualityRecorder: Stage 4 pre-write hook
//    runs the scorer and persists one report per block.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeSyncHistory implements SyncHistoryRecorder. Records every
// invocation so tests can assert on the (tenant, source, runID,
// status, processed, failed) tuple.
type fakeSyncHistory struct {
	mu       sync.Mutex
	starts   []syncStartCall
	finishes []syncFinishCall
}

type syncStartCall struct {
	tenant string
	source string
	runID  string
}

type syncFinishCall struct {
	tenant    string
	source    string
	runID     string
	status    SyncStatus
	processed int
	failed    int
}

func (f *fakeSyncHistory) Start(_ context.Context, tenantID, sourceID, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, syncStartCall{tenantID, sourceID, runID})
	return nil
}

func (f *fakeSyncHistory) Finish(_ context.Context, tenantID, sourceID, runID string, status SyncStatus, processed, failed int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, syncFinishCall{tenantID, sourceID, runID, status, processed, failed})
	return nil
}

func TestCoordinator_SyncHistory_StartOnKickoff(t *testing.T) {
	t.Parallel()
	rec := &fakeSyncHistory{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b1", Text: "hello"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)
	cfg.SyncHistory = rec
	cfg.SyncRunIDGen = func() string { return "run-1" }

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Backfill kickoff opens the row.
	kickoff := IngestEvent{
		Kind:       EventReindex,
		TenantID:   "t-1",
		SourceID:   "src-1",
		DocumentID: KickoffDocumentID("src-1"),
		SyncMode:   SyncModeBackfill,
	}
	if err := c.Submit(ctx, kickoff); err != nil {
		t.Fatalf("Submit kickoff: %v", err)
	}

	// Two backfill document events: one success (Stage 4 store
	// returns nil), one failure (we route via the fake store err
	// path on the second submit by switching mid-stream).
	doc1 := IngestEvent{Kind: EventDocumentChanged, TenantID: "t-1", SourceID: "src-1", DocumentID: "d-1", SyncMode: SyncModeBackfill}
	if err := c.Submit(ctx, doc1); err != nil {
		t.Fatalf("Submit doc-1: %v", err)
	}

	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.starts) != 1 {
		t.Fatalf("starts: %d want 1", len(rec.starts))
	}
	if rec.starts[0].runID != "run-1" || rec.starts[0].tenant != "t-1" || rec.starts[0].source != "src-1" {
		t.Fatalf("start row: %+v", rec.starts[0])
	}
}

// TestCoordinator_SyncHistory_ProcessedAndFailedCounters confirms
// processed/failed counters flow from the Stage 4 outcome paths
// into FinishBackfillRun's payload.
func TestCoordinator_SyncHistory_ProcessedAndFailedCounters(t *testing.T) {
	t.Parallel()
	rec := &fakeSyncHistory{}
	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, doc *Document) ([]Block, error) {
			// Deterministic routing keyed on DocumentID so
			// Submit ordering can't race the parse stage.
			if strings.HasPrefix(doc.DocumentID, "bad-") {
				return nil, errors.New("boom parse")
			}
			return []Block{{BlockID: "b", Text: "ok"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		store,
	)
	cfg.SyncHistory = rec
	cfg.SyncRunIDGen = func() string { return "run-2" }
	// Squeeze retry budget so the failing event lands in DLQ fast.
	cfg.MaxAttempts = 1

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	events := []IngestEvent{
		{Kind: EventReindex, TenantID: "t", SourceID: "s", DocumentID: KickoffDocumentID("s"), SyncMode: SyncModeBackfill},
		{Kind: EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "ok-1", SyncMode: SyncModeBackfill},
		{Kind: EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "bad-1", SyncMode: SyncModeBackfill},
		{Kind: EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "bad-2", SyncMode: SyncModeBackfill},
	}
	for _, evt := range events {
		if err := c.Submit(ctx, evt); err != nil {
			t.Fatalf("Submit %s: %v", evt.DocumentID, err)
		}
	}

	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Caller closes the run.
	if err := c.FinishBackfillRun(context.Background(), "t", "s", SyncStatusSucceeded); err != nil {
		t.Fatalf("FinishBackfillRun: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.finishes) != 1 {
		t.Fatalf("finishes: %d want 1", len(rec.finishes))
	}
	got := rec.finishes[0]
	if got.tenant != "t" || got.source != "s" || got.runID != "run-2" || got.status != SyncStatusSucceeded {
		t.Fatalf("finish: %+v", got)
	}
	if got.processed != 1 {
		t.Fatalf("processed: %d want 1", got.processed)
	}
	if got.failed != 2 {
		t.Fatalf("failed: %d want 2", got.failed)
	}
}

// fakeChunkQualityRecorder is the in-memory ChunkQualityRecorder
// used by the chunk-scoring hook test.
type fakeChunkQualityRecorder struct {
	mu      sync.Mutex
	reports []ChunkQualityReport
}

func (f *fakeChunkQualityRecorder) Record(_ context.Context, report ChunkQualityReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, report)
	return nil
}

func TestCoordinator_ChunkQualityHook_ScoresAndPersists(t *testing.T) {
	t.Parallel()
	cqRec := &fakeChunkQualityRecorder{}
	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			// Three blocks of different sizes so the
			// per-axis scores diverge.
			return []Block{
				{BlockID: "b-short", Text: "tiny"},
				{BlockID: "b-mid", Text: longText(800)},
				{BlockID: "b-long", Text: longText(8000)},
			}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			out := make([][]float32, len(blocks))
			for i := range blocks {
				out[i] = []float32{1, 0, 0}
			}
			return out, "m", nil
		}},
		store,
	)
	cfg.ChunkScorer = NewChunkScorer()
	cfg.ChunkQuality = cqRec

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentChanged, TenantID: "t-q", SourceID: "src-q", DocumentID: "doc-q"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	cqRec.mu.Lock()
	defer cqRec.mu.Unlock()
	if len(cqRec.reports) != 3 {
		t.Fatalf("reports: %d want 3", len(cqRec.reports))
	}
	chunkIDs := map[string]ChunkQualityReport{}
	for _, r := range cqRec.reports {
		chunkIDs[r.ChunkID] = r
		if r.TenantID != "t-q" || r.SourceID != "src-q" || r.DocumentID != "doc-q" {
			t.Fatalf("identity: %+v", r)
		}
		if r.QualityScore < 0 || r.QualityScore > 1 {
			t.Fatalf("score out of range: %+v", r)
		}
	}
	// Short block should score lower on length than the
	// mid-sized block.
	if chunkIDs["b-short"].LengthScore >= chunkIDs["b-mid"].LengthScore {
		t.Fatalf("short length should score below mid: %+v %+v", chunkIDs["b-short"], chunkIDs["b-mid"])
	}
}

// longText returns a deterministic string of length n.
func longText(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return string(b)
}

// failingChunkQualityRecorder always returns a write error. Used
// by Round-11 Task 4 to assert the observability counter
// increments without blocking the pipeline.
type failingChunkQualityRecorder struct {
	mu    sync.Mutex
	calls int
}

func (f *failingChunkQualityRecorder) Record(_ context.Context, _ ChunkQualityReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return errors.New("synthetic recorder error")
}

// TestCoordinator_ChunkQualityHook_ErrorObservability — Round-11 Task 4.
//
// Asserts that Record() failures increment
// observability.ChunkQualityErrorsTotal AND do not propagate back
// to the pipeline (the document still completes Stage 4).
func TestCoordinator_ChunkQualityHook_ErrorObservability(t *testing.T) {
	t.Parallel()
	observability_ResetForTest(t)
	failer := &failingChunkQualityRecorder{}
	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{
				{BlockID: "b-1", Text: "alpha"},
				{BlockID: "b-2", Text: "beta"},
			}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			out := make([][]float32, len(blocks))
			for i := range blocks {
				out[i] = []float32{0.5, 0.5, 0}
			}
			return out, "m", nil
		}},
		store,
	)
	cfg.ChunkScorer = NewChunkScorer()
	cfg.ChunkQuality = failer

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentChanged, TenantID: "t-err", SourceID: "src-err", DocumentID: "doc-err"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Recorder was called once per block.
	failer.mu.Lock()
	if failer.calls != 2 {
		t.Fatalf("recorder calls=%d want 2", failer.calls)
	}
	failer.mu.Unlock()

	// Counter incremented twice (once per block).
	got := chunkQualityErrorCount()
	if got < 2 {
		t.Fatalf("chunk_quality_errors_total=%d want >=2", got)
	}

	// The pipeline still wrote the document to the store, which
	// is the load-bearing invariant.
	if storeCalls := storeCallCount(store); storeCalls < 1 {
		t.Fatalf("storeCalls=%d want >=1 (chunk-quality failures must not block Stage 4)", storeCalls)
	}
}

// TestCoordinator_ChunkQualityHook_TimeoutGuard — Round-11 Task 5.
//
// Injects a recorder that blocks indefinitely; the configured
// hook timeout (set via CONTEXT_ENGINE_HOOK_TIMEOUT for the test)
// must fire so the pipeline does not stall.
func TestCoordinator_ChunkQualityHook_TimeoutGuard(t *testing.T) {
	observability_ResetForTest(t)
	t.Setenv("CONTEXT_ENGINE_HOOK_TIMEOUT", "10ms")
	ResetHookTimeoutForTest()
	t.Cleanup(ResetHookTimeoutForTest)

	slow := &slowChunkQualityRecorder{}
	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b-slow", Text: "hello"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			return [][]float32{{1, 0, 0}}, "m", nil
		}},
		store,
	)
	cfg.ChunkScorer = NewChunkScorer()
	cfg.ChunkQuality = slow

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentChanged, TenantID: "t-slow", SourceID: "src-slow", DocumentID: "doc-slow"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	c.CloseInputs()

	// If the timeout guard is missing the pipeline blocks here
	// forever; t.Fatal at the per-test deadline rather than the
	// suite-wide deadline keeps the failure mode readable.
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The pipeline must have written the chunk despite the
	// slow recorder.
	if storeCalls := storeCallCount(store); storeCalls < 1 {
		t.Fatalf("storeCalls=%d want >=1 (slow recorder must not block Stage 4)", storeCalls)
	}

	if got := hookTimeoutCount("chunk_quality_record"); got < 1 {
		t.Fatalf("hook_timeouts_total{hook=chunk_quality_record}=%d want >=1", got)
	}
}

// slowChunkQualityRecorder blocks until its context is cancelled,
// simulating a stuck Postgres write.
type slowChunkQualityRecorder struct{}

func (s *slowChunkQualityRecorder) Record(ctx context.Context, _ ChunkQualityReport) error {
	<-ctx.Done()
	return ctx.Err()
}

// slowSyncHistory blocks until its context is cancelled.
type slowSyncHistory struct{}

func (s *slowSyncHistory) Start(ctx context.Context, _ string, _ string, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *slowSyncHistory) Finish(ctx context.Context, _ string, _ string, _ string, _ SyncStatus, _ int, _ int) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestCoordinator_SyncHistory_TimeoutGuard — Round-11 Task 5.
//
// A backfill kickoff event with a slow SyncHistory recorder must
// not block the Stage-1 dispatcher beyond CONTEXT_ENGINE_HOOK_TIMEOUT.
func TestCoordinator_SyncHistory_TimeoutGuard(t *testing.T) {
	observability_ResetForTest(t)
	t.Setenv("CONTEXT_ENGINE_HOOK_TIMEOUT", "10ms")
	ResetHookTimeoutForTest()
	t.Cleanup(ResetHookTimeoutForTest)

	slow := &slowSyncHistory{}
	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b-1", Text: "a"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			return [][]float32{{1, 0, 0}}, "m", nil
		}},
		store,
	)
	cfg.SyncHistory = slow

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Kickoff event with SyncMode=backfill triggers recordSyncStart.
	kickoff := IngestEvent{
		Kind:       EventReindex,
		TenantID:   "t-slow",
		SourceID:   "src-slow",
		DocumentID: KickoffDocumentID("src-slow"),
		SyncMode:   SyncModeBackfill,
	}
	if err := c.Submit(ctx, kickoff); err != nil {
		t.Fatalf("Submit kickoff: %v", err)
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := hookTimeoutCount("sync_history_start"); got < 1 {
		t.Fatalf("hook_timeouts_total{hook=sync_history_start}=%d want >=1", got)
	}
}

func storeCallCount(store *fakeStore) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.stored)
}

func observability_ResetForTest(t *testing.T) {
	// Lazy import to avoid a package-level dep cycle.
	t.Helper()
	obsResetForTest()
}
