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
