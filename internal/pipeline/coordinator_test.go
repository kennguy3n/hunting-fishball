package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeFetch struct {
	fn func(ctx context.Context, evt IngestEvent) (*Document, error)
}

func (f fakeFetch) FetchEvent(ctx context.Context, evt IngestEvent) (*Document, error) {
	return f.fn(ctx, evt)
}

type fakeParse struct {
	fn func(ctx context.Context, doc *Document) ([]Block, error)
}

func (f fakeParse) Parse(ctx context.Context, doc *Document) ([]Block, error) {
	return f.fn(ctx, doc)
}

type fakeEmbed struct {
	fn func(ctx context.Context, tenantID string, blocks []Block) ([][]float32, string, error)
}

func (f fakeEmbed) EmbedBlocks(ctx context.Context, tenantID string, blocks []Block) ([][]float32, string, error) {
	return f.fn(ctx, tenantID, blocks)
}

type fakeStore struct {
	mu       sync.Mutex
	stored   []storedCall
	deleted  []string
	storeErr error
}

type storedCall struct {
	doc        *Document
	blocks     []Block
	embeddings [][]float32
	modelID    string
}

func (f *fakeStore) Store(_ context.Context, doc *Document, blocks []Block, em [][]float32, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return f.storeErr
	}
	f.stored = append(f.stored, storedCall{doc: doc, blocks: blocks, embeddings: em, modelID: model})

	return nil
}

func (f *fakeStore) Delete(_ context.Context, _, documentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, documentID)

	return nil
}

func newFastConfig(fetch FetchStage, parse ParseStage, embed EmbedStage, store StoreStage) CoordinatorConfig {
	return CoordinatorConfig{
		Fetch:          fetch,
		Parse:          parse,
		Embed:          embed,
		Store:          store,
		QueueSize:      4,
		MaxAttempts:    3,
		InitialBackoff: time.Microsecond,
		MaxBackoff:     time.Microsecond,
	}
}

func TestCoordinator_FullPipeline(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h1"}, nil
		}},
		fakeParse{fn: func(_ context.Context, doc *Document) ([]Block, error) {
			return []Block{{BlockID: "b1", Text: doc.DocumentID + "-b1"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			out := make([][]float32, len(blocks))
			for i := range blocks {
				out[i] = []float32{float32(i)}
			}
			return out, "test-model", nil
		}},
		store,
	)

	successes := atomic.Int32{}
	cfg.OnSuccess = func(_ context.Context, _ IngestEvent) { successes.Add(1) }

	c, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	const n = 5
	for i := 0; i < n; i++ {
		if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentChanged, TenantID: "t", SourceID: "s", DocumentID: "d-x"}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	c.CloseInputs()

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.stored; len(got) != n {
		t.Fatalf("stored: %d, want %d", len(got), n)
	}
	if got := successes.Load(); got != n {
		t.Fatalf("OnSuccess: %d, want %d", got, n)
	}
	if got := c.Metrics.Completed.Load(); got != n {
		t.Fatalf("Metrics.Completed: %d", got)
	}
}

func TestCoordinator_RetriesTransientErrors(t *testing.T) {
	t.Parallel()

	var parseAttempts atomic.Int32
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, _ IngestEvent) (*Document, error) {
			return &Document{TenantID: "t", DocumentID: "d", ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			n := parseAttempts.Add(1)
			if n < 3 {
				return nil, errors.New("transient parse error")
			}
			return []Block{{BlockID: "b", Text: "ok"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)

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
	if got := parseAttempts.Load(); got != 3 {
		t.Fatalf("parseAttempts: %d, want 3", got)
	}
}

func TestCoordinator_PoisonRoutesToDLQ(t *testing.T) {
	t.Parallel()

	var dlqCalls atomic.Int32
	var lastErr atomic.Pointer[error]
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, _ IngestEvent) (*Document, error) {
			return nil, ErrPoisonMessage
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, nil }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) { return nil, "", nil }},
		&fakeStore{},
	)
	cfg.OnDLQ = func(_ context.Context, _ IngestEvent, err error) {
		dlqCalls.Add(1)
		lastErr.Store(&err)
	}

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
	if got := dlqCalls.Load(); got != 1 {
		t.Fatalf("DLQ calls: %d", got)
	}
	if got := c.Metrics.DLQ.Load(); got != 1 {
		t.Fatalf("Metrics.DLQ: %d", got)
	}
}

func TestCoordinator_ContentHashShortCircuit(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, _ IngestEvent) (*Document, error) {
			return nil, ErrUnchanged
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, errors.New("must not call") }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return nil, "", errors.New("must not call")
		}},
		store,
	)
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
	if len(store.stored) != 0 {
		t.Fatalf("storer was called: %d", len(store.stored))
	}
	if got := c.Metrics.Skipped.Load(); got != 1 {
		t.Fatalf("Skipped: %d", got)
	}
}

func TestCoordinator_DeleteEvent(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	cfg := newFastConfig(
		// Mirror the real Fetcher: reject envelopes that have no
		// FetchURL/InlineContent. Delete/purge events carry neither, so
		// any path that still calls Fetch for them ends in the DLQ —
		// which surfaces the regression caught by Devin Review #3212906604.
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			if evt.FetchURL == "" && len(evt.InlineContent) == 0 {
				return nil, ErrPoisonMessage
			}

			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, errors.New("must not parse") }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return nil, "", errors.New("must not embed")
		}},
		store,
	)
	c, _ := NewCoordinator(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentDeleted, TenantID: "t", DocumentID: "d"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "d" {
		t.Fatalf("deleted: %+v", store.deleted)
	}
	if got := c.Metrics.DLQ.Load(); got != 0 {
		t.Fatalf("delete event reached DLQ (count=%d) — Stage 1 must bypass Fetch", got)
	}
}

// recordingGraphRAG is a GraphRAGStage fake that just records which
// methods were invoked. It does not implement the gRPC contract;
// it's used by TestCoordinator_DeleteEventPrunesGraph below to lock
// in the wiring between the coordinator's Stage 3 worker and the
// graph cleanup hook (Devin Review #3214970062).
type recordingGraphRAG struct {
	mu      sync.Mutex
	deletes []recordedDelete
	enrichs int
}

type recordedDelete struct {
	tenantID, documentID string
}

func (r *recordingGraphRAG) Enrich(_ context.Context, _ *Document, _ []Block) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enrichs++
	return nil
}

func (r *recordingGraphRAG) Delete(_ context.Context, tenantID, documentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, recordedDelete{tenantID: tenantID, documentID: documentID})
	return nil
}

// TestCoordinator_DeleteEventPrunesGraph verifies that
// EventDocumentDeleted / EventPurge events drive a GraphRAG.Delete
// call in addition to Store.Delete. Pre-fix the Stage 3 worker
// short-circuited delete events past the GraphRAG hook entirely, so
// FalkorDB nodes/edges from the deleted document were orphaned
// permanently after Stage 4 cleaned up Postgres + Qdrant.
func TestCoordinator_DeleteEventPrunesGraph(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	graphRAG := &recordingGraphRAG{}
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			if evt.FetchURL == "" && len(evt.InlineContent) == 0 {
				return nil, ErrPoisonMessage
			}
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, errors.New("must not parse") }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) {
			return nil, "", errors.New("must not embed")
		}},
		store,
	)
	cfg.GraphRAG = graphRAG
	c, _ := NewCoordinator(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentDeleted, TenantID: "tenant-a", DocumentID: "doc-1"}); err != nil {
		t.Fatalf("Submit delete: %v", err)
	}
	if err := c.Submit(ctx, IngestEvent{Kind: EventPurge, TenantID: "tenant-a", DocumentID: "doc-2"}); err != nil {
		t.Fatalf("Submit purge: %v", err)
	}
	c.CloseInputs()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	graphRAG.mu.Lock()
	defer graphRAG.mu.Unlock()
	if len(graphRAG.deletes) != 2 {
		t.Fatalf("graphRAG.deletes = %d, want 2 (one per delete/purge event)", len(graphRAG.deletes))
	}
	if graphRAG.enrichs != 0 {
		t.Fatalf("graphRAG.enrichs = %d, want 0 — delete/purge must not enrich", graphRAG.enrichs)
	}
	want := map[string]bool{"doc-1": false, "doc-2": false}
	for _, d := range graphRAG.deletes {
		if d.tenantID != "tenant-a" {
			t.Fatalf("delete tenant = %q", d.tenantID)
		}
		if _, ok := want[d.documentID]; !ok {
			t.Fatalf("unexpected document = %q", d.documentID)
		}
		want[d.documentID] = true
	}
	for doc, seen := range want {
		if !seen {
			t.Fatalf("graphRAG.Delete not called for %q", doc)
		}
	}
}

func TestCoordinator_GracefulShutdown(t *testing.T) {
	t.Parallel()

	cfg := newFastConfig(
		fakeFetch{fn: func(ctx context.Context, _ IngestEvent) (*Document, error) {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
			}
			return &Document{TenantID: "t", DocumentID: "d"}, ctx.Err()
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, nil }},
		fakeEmbed{fn: func(_ context.Context, _ string, _ []Block) ([][]float32, string, error) { return nil, "", nil }},
		&fakeStore{},
	)
	c, _ := NewCoordinator(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on cancel")
	}
}

func TestCoordinator_BackPressure(t *testing.T) {
	t.Parallel()

	// Block the store stage so the channels back up.
	storeReady := make(chan struct{})
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return []Block{{BlockID: "b", Text: "x"}}, nil }},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			return [][]float32{{1}}, "m", nil
		}},
		&blockingStore{ready: storeReady},
	)
	cfg.QueueSize = 1
	c, _ := NewCoordinator(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Submit until back-pressure blocks. Use a short ctx for the
	// would-be-blocked Submit.
	var submitted atomic.Int32
	go func() {
		for i := 0; i < 100; i++ {
			if err := c.Submit(ctx, IngestEvent{Kind: EventDocumentChanged, TenantID: "t", DocumentID: "d"}); err != nil {
				return
			}
			submitted.Add(1)
		}
	}()
	time.Sleep(50 * time.Millisecond)

	// QueueSize=1 means at most ~5 items can be in flight before back-
	// pressure kicks in (1 stage1 + 1 stage2 + 1 stage3 + 1 stage4 +
	// the worker holding one each). Empirically should be < 100 with
	// the store blocked.
	if got := submitted.Load(); got >= 100 {
		t.Fatalf("expected back-pressure, submitted=%d", got)
	}

	close(storeReady)
	cancel()
	<-done
}

type blockingStore struct {
	ready chan struct{}
}

func (b *blockingStore) Store(ctx context.Context, _ *Document, _ []Block, _ [][]float32, _ string) error {
	select {
	case <-b.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingStore) Delete(_ context.Context, _, _ string) error { return nil }

func TestCoordinator_NewCoordinator_Validation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  CoordinatorConfig
	}{
		{"nil fetch", CoordinatorConfig{Parse: fakeParse{}, Embed: fakeEmbed{}, Store: &fakeStore{}}},
		{"nil parse", CoordinatorConfig{Fetch: fakeFetch{}, Embed: fakeEmbed{}, Store: &fakeStore{}}},
		{"nil embed", CoordinatorConfig{Fetch: fakeFetch{}, Parse: fakeParse{}, Store: &fakeStore{}}},
		{"nil store", CoordinatorConfig{Fetch: fakeFetch{}, Parse: fakeParse{}, Embed: fakeEmbed{}}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewCoordinator(tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// TestCoordinator_StageTimeout_FetchExceedsBudget — Round-9 Task 8.
// A fetch that exceeds its configured per-stage timeout returns
// context.DeadlineExceeded from the inner ctx and the coordinator
// counts the event toward the DLQ after MaxAttempts retries. The
// stage timeout is local to each attempt: if fetch takes 50ms but
// timeout is 5ms, the attempt aborts at 5ms, not at 50ms.
func TestCoordinator_StageTimeout_FetchExceedsBudget(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	cfg := newFastConfig(
		fakeFetch{fn: func(ctx context.Context, _ IngestEvent) (*Document, error) {
			// Block until ctx is cancelled or 1s elapses.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(1 * time.Second):
				return &Document{}, nil
			}
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) { return nil, nil }},
		fakeEmbed{},
		store,
	)
	cfg.FetchTimeout = 10 * time.Millisecond
	cfg.MaxAttempts = 1 // single attempt → quick failure
	dlqCount := 0
	cfg.OnDLQ = func(_ context.Context, _ IngestEvent, _ error) { dlqCount++ }

	coord, err := NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runCh := make(chan error, 1)
	go func() { runCh <- coord.Run(ctx) }()
	if err := coord.Submit(ctx, IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d-timeout",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if int(coord.Metrics.DLQ.Load()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-runCh
	if got := int(coord.Metrics.DLQ.Load()); got != 1 {
		t.Fatalf("expected 1 DLQ entry after fetch timeout; got %d", got)
	}
}

// TestCoordinatorConfig_LoadStageTimeoutsFromEnv parses Go duration
// strings from the env and applies them to the config. Invalid /
// missing values fall back to zero.
func TestCoordinatorConfig_LoadStageTimeoutsFromEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"CONTEXT_ENGINE_FETCH_TIMEOUT": "30s",
		"CONTEXT_ENGINE_PARSE_TIMEOUT": "1m",
		"CONTEXT_ENGINE_EMBED_TIMEOUT": "bad-value",
		// STORE not set
	}
	cfg := &CoordinatorConfig{}
	cfg.LoadStageTimeoutsFromEnv(func(k string) string { return env[k] })
	if cfg.FetchTimeout != 30*time.Second {
		t.Fatalf("FetchTimeout: %v", cfg.FetchTimeout)
	}
	if cfg.ParseTimeout != time.Minute {
		t.Fatalf("ParseTimeout: %v", cfg.ParseTimeout)
	}
	if cfg.EmbedTimeout != 0 {
		t.Fatalf("EmbedTimeout: bad value should fall back to 0; got %v", cfg.EmbedTimeout)
	}
	if cfg.StoreTimeout != 0 {
		t.Fatalf("StoreTimeout: unset should be 0; got %v", cfg.StoreTimeout)
	}
}

// TestCoordinator_StageBreaker_ShortCircuits — Round-13 Task 5.
//
// When the embed-stage breaker is configured and pre-tripped, the
// coordinator must route incoming events to the DLQ without
// invoking the embed function at all.
func TestCoordinator_StageBreaker_ShortCircuits(t *testing.T) {
	t.Parallel()
	var embedCalls atomic.Int32
	cfg := newFastConfig(
		fakeFetch{fn: func(_ context.Context, evt IngestEvent) (*Document, error) {
			return &Document{TenantID: evt.TenantID, DocumentID: evt.DocumentID, Content: []byte("x"), ContentHash: "h"}, nil
		}},
		fakeParse{fn: func(_ context.Context, _ *Document) ([]Block, error) {
			return []Block{{BlockID: "b", Text: "x"}}, nil
		}},
		fakeEmbed{fn: func(_ context.Context, _ string, blocks []Block) ([][]float32, string, error) {
			embedCalls.Add(1)
			return [][]float32{{1}}, "m", nil
		}},
		&fakeStore{},
	)
	breaker, err := NewStageCircuitBreaker(StageCircuitBreakerConfig{Stage: "embed", Threshold: 1, OpenFor: time.Minute})
	if err != nil {
		t.Fatalf("NewStageCircuitBreaker: %v", err)
	}
	// Pre-trip the breaker.
	breaker.OnFailure()
	if breaker.State() != StageBreakerOpen {
		t.Fatalf("pre-trip state=%s", breaker.State())
	}
	cfg.StageBreakers = map[string]*StageCircuitBreaker{"embed": breaker}

	var dlqCalls atomic.Int32
	var dlqErr error
	var dlqMu sync.Mutex
	cfg.OnDLQ = func(_ context.Context, _ IngestEvent, err error) {
		dlqCalls.Add(1)
		dlqMu.Lock()
		defer dlqMu.Unlock()
		if dlqErr == nil {
			dlqErr = err
		}
	}
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
	if got := embedCalls.Load(); got != 0 {
		t.Fatalf("embed call count=%d; expected 0 (breaker open)", got)
	}
	if got := dlqCalls.Load(); got != 1 {
		t.Fatalf("dlq calls=%d; expected 1", got)
	}
	dlqMu.Lock()
	defer dlqMu.Unlock()
	if !errors.Is(dlqErr, ErrStageBreakerOpen) {
		t.Fatalf("dlq error=%v; expected ErrStageBreakerOpen", dlqErr)
	}
}
