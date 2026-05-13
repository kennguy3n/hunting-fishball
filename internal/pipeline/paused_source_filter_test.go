package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// paused_source_filter_test.go — Round-20 Task 17.
//
// Asserts that the coordinator short-circuits Stage 1 when the
// PausedSourceFilter reports a (tenant, source) is paused: the
// FetchStage is never invoked, the consumer's OnSuccess fires
// for offset commit, and the event counts as Skipped + Completed
// rather than DLQ'd.

// blockingFetch fails the test if it's invoked.
type blockingFetch struct {
	t       *testing.T
	invoked atomic.Int64
}

func (b *blockingFetch) FetchEvent(_ context.Context, _ IngestEvent) (*Document, error) {
	b.invoked.Add(1)
	b.t.Errorf("Fetch invoked for paused source")

	return nil, nil //nolint:nilnil // test only
}

// pausedFilter returns true for one specific tenant/source.
type pausedFilter struct {
	tenantID, sourceID string
}

func (p pausedFilter) IsPaused(_ context.Context, tid, sid string) bool {
	return p.tenantID == tid && p.sourceID == sid
}

func TestCoordinator_PausedSourceFilter_SkipsFetch(t *testing.T) {
	t.Parallel()
	bf := &blockingFetch{t: t}
	var success atomic.Int64
	c, err := NewCoordinator(CoordinatorConfig{
		Fetch:              bf,
		Parse:              stubParse{},
		Embed:              stubEmbed{},
		Store:              stubStore{},
		QueueSize:          4,
		PausedSourceFilter: pausedFilter{tenantID: "t-1", sourceID: "s-1"},
		OnSuccess: func(_ context.Context, _ IngestEvent) {
			success.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Run(ctx) }()

	if err := c.Submit(ctx, IngestEvent{TenantID: "t-1", SourceID: "s-1", DocumentID: "d-1"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Allow the coordinator a moment to drain the event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if success.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-errCh

	if bf.invoked.Load() != 0 {
		t.Fatalf("Fetch was invoked %d times for paused source", bf.invoked.Load())
	}
	if success.Load() != 1 {
		t.Fatalf("OnSuccess fired %d times, want 1", success.Load())
	}
	if got := c.Metrics.Skipped.Load(); got != 1 {
		t.Fatalf("Skipped: %d, want 1", got)
	}
	if got := c.Metrics.Completed.Load(); got != 1 {
		t.Fatalf("Completed: %d, want 1", got)
	}
	if got := c.Metrics.DLQ.Load(); got != 0 {
		t.Fatalf("DLQ: %d, want 0", got)
	}
}

func TestCoordinator_PausedSourceFilter_OnlyPausedSourceIsSkipped(t *testing.T) {
	t.Parallel()
	var fetched atomic.Int64
	fetch := FetchStageFunc(func(_ context.Context, evt IngestEvent) (*Document, error) {
		fetched.Add(1)

		return &Document{TenantID: evt.TenantID, SourceID: evt.SourceID, DocumentID: evt.DocumentID}, nil
	})
	var success atomic.Int64
	c, err := NewCoordinator(CoordinatorConfig{
		Fetch:              fetch,
		Parse:              stubParse{},
		Embed:              stubEmbed{},
		Store:              stubStore{},
		QueueSize:          4,
		PausedSourceFilter: pausedFilter{tenantID: "t-1", sourceID: "paused"},
		OnSuccess: func(_ context.Context, _ IngestEvent) {
			success.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Run(ctx) }()

	_ = c.Submit(ctx, IngestEvent{TenantID: "t-1", SourceID: "paused", DocumentID: "d-1"})
	_ = c.Submit(ctx, IngestEvent{TenantID: "t-1", SourceID: "active", DocumentID: "d-2"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if success.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-errCh

	if fetched.Load() != 1 {
		t.Fatalf("Fetch invocations: %d, want 1 (only the active source)", fetched.Load())
	}
	if got := c.Metrics.Skipped.Load(); got != 1 {
		t.Fatalf("Skipped: %d, want 1", got)
	}
}

// FetchStageFunc adapts a function value to the FetchStage interface
// for inline test fakes.
type FetchStageFunc func(ctx context.Context, evt IngestEvent) (*Document, error)

// FetchEvent implements FetchStage.
func (f FetchStageFunc) FetchEvent(ctx context.Context, evt IngestEvent) (*Document, error) {
	return f(ctx, evt)
}

// TestCoordinator_PausedSourceFilter_RecordsSyncStart asserts that
// the Stage-1 paused-source short-circuit still opens a
// sync_history row when a backfill kickoff event arrives for a
// paused source. This is the Round-22 Devin Review fix: previously
// recordSyncStart was bypassed by the paused-skip continue, so the
// admin endpoint had no visibility into the attempted run.
func TestCoordinator_PausedSourceFilter_RecordsSyncStart(t *testing.T) {
	t.Parallel()
	rec := &fakeSyncHistory{}
	bf := &blockingFetch{t: t}
	c, err := NewCoordinator(CoordinatorConfig{
		Fetch:              bf,
		Parse:              stubParse{},
		Embed:              stubEmbed{},
		Store:              stubStore{},
		QueueSize:          4,
		PausedSourceFilter: pausedFilter{tenantID: "t-1", sourceID: "s-1"},
		SyncHistory:        rec,
		SyncRunIDGen:       func() string { return "run-paused" },
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Run(ctx) }()

	kickoff := IngestEvent{
		Kind:       EventReindex,
		TenantID:   "t-1",
		SourceID:   "s-1",
		DocumentID: KickoffDocumentID("s-1"),
		SyncMode:   SyncModeBackfill,
	}
	if err := c.Submit(ctx, kickoff); err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		seen := len(rec.starts)
		rec.mu.Unlock()
		if seen >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-errCh

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.starts) != 1 {
		t.Fatalf("starts: %d, want 1 (paused-source kickoff must still open sync_history)", len(rec.starts))
	}
	if rec.starts[0].runID != "run-paused" || rec.starts[0].tenant != "t-1" || rec.starts[0].source != "s-1" {
		t.Fatalf("start row: %+v", rec.starts[0])
	}
	if bf.invoked.Load() != 0 {
		t.Fatalf("Fetch invoked %d times for paused source", bf.invoked.Load())
	}
}
