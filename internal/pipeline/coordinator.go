package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// FetchStage is the abstract Stage 1 contract the coordinator depends
// on. *Fetcher satisfies it; tests inject lighter fakes.
type FetchStage interface {
	FetchEvent(ctx context.Context, evt IngestEvent) (*Document, error)
}

// ParseStage is the abstract Stage 2 contract.
type ParseStage interface {
	Parse(ctx context.Context, doc *Document) ([]Block, error)
}

// EmbedStage is the abstract Stage 3 contract.
type EmbedStage interface {
	EmbedBlocks(ctx context.Context, tenantID string, blocks []Block) ([][]float32, string, error)
}

// StoreStage is the abstract Stage 4 contract.
type StoreStage interface {
	Store(ctx context.Context, doc *Document, blocks []Block, embeddings [][]float32, modelID string) error
	Delete(ctx context.Context, tenantID, documentID string) error
}

// StageConfig sizes the per-stage worker pools. Phase 8 introduces
// bounded parallelism per stage — without this, every stage would
// run a single goroutine and the embedder (which is normally the
// long-pole) would dominate end-to-end latency.
//
// All four fields default to 1 (sequential per-stage processing,
// matching the original coordinator semantics). Setting them above
// 1 spawns N goroutines that each read from the upstream stage
// channel; ordering across documents is no longer preserved within
// a stage when N>1, but ordering between stages is still
// channel-enforced (Stage 2 cannot see a document Stage 1 has not
// pushed).
type StageConfig struct {
	FetchWorkers int `json:"fetch_workers"`
	ParseWorkers int `json:"parse_workers"`
	EmbedWorkers int `json:"embed_workers"`
	StoreWorkers int `json:"store_workers"`
}

func (s *StageConfig) defaults() {
	if s.FetchWorkers < 1 {
		s.FetchWorkers = 1
	}
	if s.ParseWorkers < 1 {
		s.ParseWorkers = 1
	}
	if s.EmbedWorkers < 1 {
		s.EmbedWorkers = 1
	}
	if s.StoreWorkers < 1 {
		s.StoreWorkers = 1
	}
}

// CoordinatorConfig configures the 4-stage pipeline coordinator.
type CoordinatorConfig struct {
	Fetch FetchStage
	Parse ParseStage
	Embed EmbedStage
	Store StoreStage

	// QueueSize bounds the per-stage channel buffer; back-pressure is
	// enforced by sized buffers between stages. Defaults to 8.
	QueueSize int

	// Workers controls per-stage goroutine fan-out (Phase 8 Task 16).
	// Zero values default to 1 (preserves pre-Phase-8 semantics).
	Workers StageConfig

	// MaxAttempts is the per-document retry budget. Defaults to 3.
	MaxAttempts int

	// InitialBackoff is the first retry sleep. Defaults to 200ms.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential backoff. Defaults to 5s.
	MaxBackoff time.Duration

	// OnDLQ is invoked once per poison / exhausted-retry message. The
	// production wiring publishes to a Kafka DLQ topic; tests collect
	// in-memory.
	OnDLQ func(ctx context.Context, evt IngestEvent, err error)

	// OnSuccess is invoked once per successfully-processed message
	// just before offset commit. Used by the consumer to advance the
	// committed offset.
	OnSuccess func(ctx context.Context, evt IngestEvent)
}

func (c *CoordinatorConfig) defaults() {
	if c.QueueSize == 0 {
		c.QueueSize = 8
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.InitialBackoff == 0 {
		c.InitialBackoff = 200 * time.Millisecond
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 5 * time.Second
	}
	c.Workers.defaults()
}

// CoordinatorMetrics counts the events that flow through the
// coordinator. Exposed primarily for testing.
type CoordinatorMetrics struct {
	Submitted atomic.Int64
	Completed atomic.Int64
	Skipped   atomic.Int64 // unchanged content_hash
	DLQ       atomic.Int64
}

// Coordinator wires Stage 1 → Stage 2 → Stage 3 → Stage 4 with
// channel-based back-pressure.
type Coordinator struct {
	cfg     CoordinatorConfig
	Metrics CoordinatorMetrics

	mu      sync.Mutex
	running bool

	stage1 chan stagedEvent
	stage2 chan stagedItem
	stage3 chan stagedItem
	stage4 chan stagedItem
}

type stagedEvent struct {
	evt IngestEvent
}

type stagedItem struct {
	evt        IngestEvent
	doc        *Document
	blocks     []Block
	embeddings [][]float32
	modelID    string
}

// NewCoordinator builds a Coordinator. Returns an error if any stage
// is nil. Channels are allocated up front so Submit can block on them
// before Run is called (back-pressure semantics from the very first
// event).
func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	if cfg.Fetch == nil {
		return nil, errors.New("coordinator: nil Fetch")
	}
	if cfg.Parse == nil {
		return nil, errors.New("coordinator: nil Parse")
	}
	if cfg.Embed == nil {
		return nil, errors.New("coordinator: nil Embed")
	}
	if cfg.Store == nil {
		return nil, errors.New("coordinator: nil Store")
	}
	cfg.defaults()

	c := &Coordinator{cfg: cfg}
	c.stage1 = make(chan stagedEvent, c.cfg.QueueSize)
	c.stage2 = make(chan stagedItem, c.cfg.QueueSize)
	c.stage3 = make(chan stagedItem, c.cfg.QueueSize)
	c.stage4 = make(chan stagedItem, c.cfg.QueueSize)

	return c, nil
}

// Run starts the coordinator goroutines. The supplied context drives
// graceful shutdown — cancelling ctx drains the in-flight items and
// returns. Run blocks until all stages exit; the returned error is
// any non-nil error returned by a stage goroutine (errgroup
// semantics).
func (c *Coordinator) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()

		return errors.New("coordinator: already running")
	}
	c.running = true
	c.mu.Unlock()

	g, gctx := errgroup.WithContext(ctx)

	// Run N workers for a stage; the closer goroutine waits for all
	// workers in the stage to finish before closing the downstream
	// channel. This generalises the original "1 worker per stage"
	// design to N workers per stage (Phase 8 Task 16).
	stage := func(workers int, downstream chan<- stagedItem, body func()) {
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			g.Go(func() error {
				defer wg.Done()
				body()
				return nil
			})
		}
		if downstream != nil {
			g.Go(func() error {
				wg.Wait()
				close(downstream)
				return nil
			})
		} else {
			// Stage 4 has no downstream channel; we still need a
			// goroutine to keep the errgroup tracking workers alive
			// until they exit, but we don't close anything.
			g.Go(func() error {
				wg.Wait()
				return nil
			})
		}
	}

	// Stage 1 workers: Fetch
	stage(c.cfg.Workers.FetchWorkers, c.stage2, func() {
		for {
			select {
			case <-gctx.Done():
				return
			case se, ok := <-c.stage1:
				if !ok {
					return
				}
				if se.evt.Kind == EventDocumentDeleted || se.evt.Kind == EventPurge {
					doc := &Document{
						TenantID:   se.evt.TenantID,
						SourceID:   se.evt.SourceID,
						DocumentID: se.evt.DocumentID,
					}
					select {
					case <-gctx.Done():
						return
					case c.stage2 <- stagedItem{evt: se.evt, doc: doc}:
					}

					continue
				}
				doc, err := c.runWithRetry(gctx, "fetch", func(rc context.Context) (any, error) {
					return c.cfg.Fetch.FetchEvent(rc, se.evt)
				})
				if errors.Is(err, ErrUnchanged) {
					c.Metrics.Skipped.Add(1)
					c.Metrics.Completed.Add(1)
					if c.cfg.OnSuccess != nil {
						c.cfg.OnSuccess(gctx, se.evt)
					}

					continue
				}
				if err != nil {
					c.routeDLQ(gctx, se.evt, err)

					continue
				}
				select {
				case <-gctx.Done():
					return
				case c.stage2 <- stagedItem{evt: se.evt, doc: doc.(*Document)}:
				}
			}
		}
	})

	// Stage 2 workers: Parse
	stage(c.cfg.Workers.ParseWorkers, c.stage3, func() {
		for {
			select {
			case <-gctx.Done():
				return
			case it, ok := <-c.stage2:
				if !ok {
					return
				}
				if it.evt.Kind == EventDocumentDeleted || it.evt.Kind == EventPurge {
					select {
					case <-gctx.Done():
						return
					case c.stage3 <- it:
					}

					continue
				}
				blocks, err := c.runWithRetry(gctx, "parse", func(rc context.Context) (any, error) {
					return c.cfg.Parse.Parse(rc, it.doc)
				})
				if err != nil {
					c.routeDLQ(gctx, it.evt, err)

					continue
				}
				it.blocks = blocks.([]Block)
				select {
				case <-gctx.Done():
					return
				case c.stage3 <- it:
				}
			}
		}
	})

	// Stage 3 workers: Embed
	stage(c.cfg.Workers.EmbedWorkers, c.stage4, func() {
		for {
			select {
			case <-gctx.Done():
				return
			case it, ok := <-c.stage3:
				if !ok {
					return
				}
				if len(it.blocks) == 0 {
					select {
					case <-gctx.Done():
						return
					case c.stage4 <- it:
					}

					continue
				}
				type embedResult struct {
					em      [][]float32
					modelID string
				}
				out, err := c.runWithRetry(gctx, "embed", func(rc context.Context) (any, error) {
					em, model, err := c.cfg.Embed.EmbedBlocks(rc, it.evt.TenantID, it.blocks)
					if err != nil {
						return nil, err
					}

					return embedResult{em: em, modelID: model}, nil
				})
				if err != nil {
					c.routeDLQ(gctx, it.evt, err)

					continue
				}
				res := out.(embedResult)
				it.embeddings = res.em
				it.modelID = res.modelID
				select {
				case <-gctx.Done():
					return
				case c.stage4 <- it:
				}
			}
		}
	})

	// Stage 4 workers: Store
	stage(c.cfg.Workers.StoreWorkers, nil, func() {
		for {
			select {
			case <-gctx.Done():
				return
			case it, ok := <-c.stage4:
				if !ok {
					return
				}
				if it.evt.Kind == EventDocumentDeleted || it.evt.Kind == EventPurge {
					if _, err := c.runWithRetry(gctx, "delete", func(rc context.Context) (any, error) {
						return nil, c.cfg.Store.Delete(rc, it.evt.TenantID, it.evt.DocumentID)
					}); err != nil {
						c.routeDLQ(gctx, it.evt, err)

						continue
					}
				} else {
					if _, err := c.runWithRetry(gctx, "store", func(rc context.Context) (any, error) {
						return nil, c.cfg.Store.Store(rc, it.doc, it.blocks, it.embeddings, it.modelID)
					}); err != nil {
						c.routeDLQ(gctx, it.evt, err)

						continue
					}
				}
				c.Metrics.Completed.Add(1)
				if c.cfg.OnSuccess != nil {
					c.cfg.OnSuccess(gctx, it.evt)
				}
			}
		}
	})

	err := g.Wait()
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return nil
}

// Submit pushes an event onto the Stage 1 channel. Blocks if the
// channel is full (back-pressure to the consumer). Returns ctx.Err()
// when ctx fires.
//
// Submit is safe to call before Run starts; the channels are allocated
// in NewCoordinator and Run drains them.
func (c *Coordinator) Submit(ctx context.Context, evt IngestEvent) error {
	c.Metrics.Submitted.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.stage1 <- stagedEvent{evt: evt}:
		return nil
	}
}

// CloseInputs signals that no more events will be submitted; the
// coordinator drains the in-flight pipeline and returns from Run.
//
// CloseInputs must be called exactly once per Run; calling it twice
// panics on the underlying channel close.
func (c *Coordinator) CloseInputs() {
	close(c.stage1)
}

// runWithRetry executes fn with bounded retries on transient errors.
// Poison messages (ErrPoisonMessage) and ErrUnchanged are returned
// immediately without retry.
func (c *Coordinator) runWithRetry(ctx context.Context, stage string, fn func(context.Context) (any, error)) (any, error) {
	ctx, span := observability.StartSpan(ctx, "pipeline."+stage,
		observability.AttrStage.String(stage),
	)
	defer span.End()

	var lastErr error
	backoff := c.cfg.InitialBackoff
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		out, err := fn(ctx)
		if err == nil {
			return out, nil
		}
		if errors.Is(err, ErrUnchanged) {
			return nil, err
		}
		if errors.Is(err, ErrPoisonMessage) {
			observability.RecordError(span, err)
			return nil, err
		}
		lastErr = err
		if attempt == c.cfg.MaxAttempts {
			observability.RecordError(span, err)
			break
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()

			return nil, ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}

	return nil, fmt.Errorf("retries exhausted: %w", lastErr)
}

// routeDLQ delivers a failed event to the configured DLQ sink. Always
// counts the failure; the DLQ callback itself is best-effort.
func (c *Coordinator) routeDLQ(ctx context.Context, evt IngestEvent, err error) {
	c.Metrics.DLQ.Add(1)
	if c.cfg.OnDLQ != nil {
		c.cfg.OnDLQ(ctx, evt, err)
	}
}
