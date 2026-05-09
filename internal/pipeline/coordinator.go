package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
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

// CoordinatorConfig configures the 4-stage pipeline coordinator.
type CoordinatorConfig struct {
	Fetch FetchStage
	Parse ParseStage
	Embed EmbedStage
	Store StoreStage

	// QueueSize bounds the per-stage channel buffer; back-pressure is
	// enforced by sized buffers between stages. Defaults to 8.
	QueueSize int

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

	// Stage 1 worker: Fetch
	g.Go(func() error {
		defer close(c.stage2)
		for {
			select {
			case <-gctx.Done():
				return nil
			case se, ok := <-c.stage1:
				if !ok {
					return nil
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
					return nil
				case c.stage2 <- stagedItem{evt: se.evt, doc: doc.(*Document)}:
				}
			}
		}
	})

	// Stage 2 worker: Parse
	g.Go(func() error {
		defer close(c.stage3)
		for {
			select {
			case <-gctx.Done():
				return nil
			case it, ok := <-c.stage2:
				if !ok {
					return nil
				}
				if it.evt.Kind == EventDocumentDeleted || it.evt.Kind == EventPurge {
					// Skip Parse / Embed; route directly to Stage 4.
					select {
					case <-gctx.Done():
						return nil
					case c.stage4 <- it:
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
					return nil
				case c.stage3 <- it:
				}
			}
		}
	})

	// Stage 3 worker: Embed
	g.Go(func() error {
		defer close(c.stage4)
		for {
			select {
			case <-gctx.Done():
				return nil
			case it, ok := <-c.stage3:
				if !ok {
					return nil
				}
				if len(it.blocks) == 0 {
					// Empty parse → still push to Stage 4 so Storer can
					// no-op cleanly; this preserves the invariant that
					// every accepted event hits Stage 4 exactly once.
					select {
					case <-gctx.Done():
						return nil
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
					return nil
				case c.stage4 <- it:
				}
			}
		}
	})

	// Stage 4 worker: Store
	g.Go(func() error {
		for {
			select {
			case <-gctx.Done():
				return nil
			case it, ok := <-c.stage4:
				if !ok {
					return nil
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
func (c *Coordinator) runWithRetry(ctx context.Context, _ string, fn func(context.Context) (any, error)) (any, error) {
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
			return nil, err
		}
		lastErr = err
		if attempt == c.cfg.MaxAttempts {
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
