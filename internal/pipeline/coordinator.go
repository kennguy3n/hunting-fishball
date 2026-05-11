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

// EmbedStageBySource is the optional per-source variant of EmbedStage.
// When the configured Embed implementation also satisfies this
// interface, the coordinator routes through it so the embed worker
// can pick a per-source model (Round-6 Task 1 / Round-8 Task 3).
type EmbedStageBySource interface {
	EmbedBlocksForSource(ctx context.Context, tenantID, sourceID string, blocks []Block) ([][]float32, string, error)
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

	// GraphRAG is the optional Stage 3b enrichment hook (entity +
	// relation extraction → per-tenant FalkorDB graph). When nil
	// the coordinator runs the original 4-stage pipeline. When
	// non-nil the coordinator calls Enrich after Stage 3 and before
	// Stage 4 — failures inside Enrich are logged and ignored so
	// graph-side outages cannot block ingestion. See
	// docs/ARCHITECTURE.md §3.3 "Pipeline coordinator".
	GraphRAG GraphRAGStage

	// PriorityBuffer, when non-nil, fronts Stage 1: Submit pushes
	// events into the buffer, and Run launches a drain goroutine
	// that pops from the buffer in priority order (high > normal
	// > low, with anti-starvation) and feeds Stage 1 (Round-6 Task
	// 8 / Round-8 Task 2). Disabled when nil.
	PriorityBuffer *PriorityBuffer

	// PriorityClassifier assigns a Priority to each Submitted
	// event. Only consulted when PriorityBuffer is non-nil.
	// Defaults to DefaultPriorityClassifier{}.
	PriorityClassifier PriorityClassifier

	// RetryAnalytics, when non-nil, receives one outcome per
	// retry attempt (success / retry / failed) per stage via
	// RecordAttempt. Used by /v1/admin/pipeline/retry-stats
	// (Round-6 Task 12 / Round-8 Task 4).
	RetryAnalytics *RetryAnalytics

	// Round-9 Task 8: per-stage timeouts. Each value, when
	// non-zero, wraps the stage's fn call in
	// context.WithTimeout. A zero value means "no timeout" and
	// preserves the pre-Round-9 behaviour where stages were
	// only bounded by the upstream ctx and the per-stage retry
	// loop's MaxAttempts/backoff. The four stage names that
	// participate are "fetch", "parse", "embed", and "store"
	// (the "delete" stage shares StoreTimeout because both
	// route through the same store backend). Operators can wire
	// these from env vars via LoadStageTimeoutsFromEnv.
	FetchTimeout time.Duration
	ParseTimeout time.Duration
	EmbedTimeout time.Duration
	StoreTimeout time.Duration

	// Round-10 Task 3: per-tenant sync-history recorder. When
	// non-nil, Stage 1 calls Start on every backfill kickoff and
	// Stage 4 / DLQ increments per-(tenant, source) counters as
	// the documents stream through. Callers close the row via
	// Coordinator.FinishBackfillRun.
	SyncHistory SyncHistoryRecorder

	// SyncRunIDGen mints the run_id stored on sync_history rows.
	// Defaults to ulid.Make().String().
	SyncRunIDGen func() string

	// ChunkScorer is the optional Round-10 Task 4 hook that the
	// store stage invokes before each chunk write so admins get a
	// per-chunk quality score persisted via ChunkQualityRecorder.
	ChunkScorer *ChunkScorer

	// ChunkQuality is the sink the store stage writes per-chunk
	// quality reports into. Best-effort: write failures are logged
	// and ignored so a scoring outage cannot block ingestion.
	ChunkQuality ChunkQualityRecorder
}

// LoadStageTimeoutsFromEnv reads the four
// CONTEXT_ENGINE_*_TIMEOUT env vars and applies them onto cfg in
// place — Round-9 Task 8. The values are parsed as Go duration
// strings (e.g. "30s", "2m"). Empty or invalid values are silently
// ignored so a misconfigured env doesn't crash the pipeline; the
// stage falls back to "no timeout".
func (c *CoordinatorConfig) LoadStageTimeoutsFromEnv(getenv func(string) string) {
	if getenv == nil {
		return
	}
	parse := func(name string) time.Duration {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return 0
		}
		return d
	}
	if d := parse("CONTEXT_ENGINE_FETCH_TIMEOUT"); d > 0 {
		c.FetchTimeout = d
	}
	if d := parse("CONTEXT_ENGINE_PARSE_TIMEOUT"); d > 0 {
		c.ParseTimeout = d
	}
	if d := parse("CONTEXT_ENGINE_EMBED_TIMEOUT"); d > 0 {
		c.EmbedTimeout = d
	}
	if d := parse("CONTEXT_ENGINE_STORE_TIMEOUT"); d > 0 {
		c.StoreTimeout = d
	}
}

// stageTimeout maps a stage name to the configured per-stage
// timeout. Returns 0 (no timeout) for unknown stages so the
// default behaviour is preserved.
func (c *CoordinatorConfig) stageTimeout(stage string) time.Duration {
	switch stage {
	case "fetch":
		return c.FetchTimeout
	case "parse":
		return c.ParseTimeout
	case "embed":
		return c.EmbedTimeout
	case "store", "delete":
		return c.StoreTimeout
	default:
		return 0
	}
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
	c.GraphRAG = graphRAGOrNoop(c.GraphRAG)
	if c.PriorityBuffer != nil && c.PriorityClassifier == nil {
		c.PriorityClassifier = DefaultPriorityClassifier{}
	}
	if c.SyncHistory != nil && c.SyncRunIDGen == nil {
		c.SyncRunIDGen = defaultSyncRunIDGen
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

	// Round-10 Task 3: per-(tenant, source) sync-run counters. See
	// syncRunState. nil when CoordinatorConfig.SyncHistory is unset.
	syncRunsMu sync.Mutex
	syncRuns   map[string]*syncRunState
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
	if c.cfg.SyncHistory != nil {
		c.syncRuns = make(map[string]*syncRunState)
	}

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

	// Round-8 Task 2: when a PriorityBuffer fronts Stage 1, launch a
	// drain goroutine that pulls events out of the buffer in
	// priority order and feeds c.stage1. We close c.stage1 once the
	// drain returns (buffer closed + drained), so the existing
	// Stage-1 workers shut down cleanly. When PriorityBuffer is nil
	// the legacy path (Submit -> c.stage1 directly, CloseInputs
	// closes c.stage1) is preserved.
	if c.cfg.PriorityBuffer != nil {
		g.Go(func() error {
			defer close(c.stage1)
			c.cfg.PriorityBuffer.Drain(gctx, func(evt IngestEvent) {
				select {
				case <-gctx.Done():
				case c.stage1 <- stagedEvent{evt: evt}:
					c.recordChannelDepths()
				}
			})
			return nil
		})
	}

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
				// Round-10 Task 3: a backfill kickoff opens a
				// sync_history row so the admin endpoint can
				// observe the run in flight.
				c.recordSyncStart(gctx, se.evt)
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
				doc, err := c.runWithRetry(gctx, "fetch", se.evt.TenantID+":"+se.evt.DocumentID, func(rc context.Context) (any, error) {
					return c.cfg.Fetch.FetchEvent(rc, se.evt)
				})
				if errors.Is(err, ErrUnchanged) {
					c.Metrics.Skipped.Add(1)
					c.Metrics.Completed.Add(1)
					c.recordSyncOutcome(se.evt, true)
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
				blocks, err := c.runWithRetry(gctx, "parse", it.evt.TenantID+":"+it.evt.DocumentID, func(rc context.Context) (any, error) {
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
				// Stage 3b deletion: prune the per-document
				// subgraph from FalkorDB so delete/purge events
				// don't leave orphan nodes/edges behind after
				// Stage 4 cleans up Postgres + Qdrant. Best-
				// effort like Enrich — the hook swallows
				// transport errors so a graph outage cannot
				// block deletion.
				if it.evt.Kind == EventDocumentDeleted || it.evt.Kind == EventPurge {
					if err := c.cfg.GraphRAG.Delete(gctx, it.evt.TenantID, it.evt.DocumentID); err != nil {
						observability.NewLogger("pipeline-graphrag").Warn(
							"graphrag stage 3b delete returned error",
							"tenant", it.evt.TenantID,
							"document", it.evt.DocumentID,
							"error", err.Error(),
						)
					}
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
				out, err := c.runWithRetry(gctx, "embed", it.evt.TenantID+":"+it.evt.DocumentID, func(rc context.Context) (any, error) {
					var (
						em    [][]float32
						model string
						err   error
					)
					if eb, ok := c.cfg.Embed.(EmbedStageBySource); ok {
						em, model, err = eb.EmbedBlocksForSource(rc, it.evt.TenantID, it.evt.SourceID, it.blocks)
					} else {
						em, model, err = c.cfg.Embed.EmbedBlocks(rc, it.evt.TenantID, it.blocks)
					}
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

				// Stage 3b: optional GraphRAG enrichment. Best-effort —
				// the hook swallows transport errors so a graph
				// outage cannot block ingestion. We re-use the
				// pipeline's own ctx so cancellation still
				// propagates.
				if it.evt.Kind != EventDocumentDeleted && it.evt.Kind != EventPurge {
					if err := c.cfg.GraphRAG.Enrich(gctx, it.doc, it.blocks); err != nil {
						observability.NewLogger("pipeline-graphrag").Warn(
							"graphrag stage 3b returned error",
							"tenant", it.evt.TenantID,
							"document", it.evt.DocumentID,
							"error", err.Error(),
						)
					}
				}

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
					if _, err := c.runWithRetry(gctx, "delete", it.evt.TenantID+":"+it.evt.DocumentID, func(rc context.Context) (any, error) {
						return nil, c.cfg.Store.Delete(rc, it.evt.TenantID, it.evt.DocumentID)
					}); err != nil {
						c.routeDLQ(gctx, it.evt, err)

						continue
					}
				} else {
					// Round-10 Task 4: per-chunk quality
					// scoring runs before the Store write so
					// the report row carries the same chunk_id
					// the chunk lands under. Best-effort — a
					// recorder write failure must not block the
					// chunk itself.
					c.scoreAndRecordBlocks(gctx, it)
					if _, err := c.runWithRetry(gctx, "store", it.evt.TenantID+":"+it.evt.DocumentID, func(rc context.Context) (any, error) {
						return nil, c.cfg.Store.Store(rc, it.doc, it.blocks, it.embeddings, it.modelID)
					}); err != nil {
						c.routeDLQ(gctx, it.evt, err)

						continue
					}
				}
				c.Metrics.Completed.Add(1)
				c.recordSyncOutcome(it.evt, true)
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
	if c.cfg.PriorityBuffer != nil {
		prio := PriorityNormal
		if c.cfg.PriorityClassifier != nil {
			prio = c.cfg.PriorityClassifier.Classify(evt)
		}
		return c.cfg.PriorityBuffer.Push(ctx, evt, prio)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.stage1 <- stagedEvent{evt: evt}:
		c.recordChannelDepths()
		return nil
	}
}

// recordChannelDepths emits the current pipeline channel depths
// for the back-pressure gauge (Round-6 Task 9). Cheap; called on
// every Submit. Length reads on Go channels are O(1).
func (c *Coordinator) recordChannelDepths() {
	observability.PipelineChannelDepth.WithLabelValues("fetch").Set(float64(len(c.stage1)))
	observability.PipelineChannelDepth.WithLabelValues("parse").Set(float64(len(c.stage2)))
	observability.PipelineChannelDepth.WithLabelValues("embed").Set(float64(len(c.stage3)))
	observability.PipelineChannelDepth.WithLabelValues("store").Set(float64(len(c.stage4)))
}

// CloseInputs signals that no more events will be submitted; the
// coordinator drains the in-flight pipeline and returns from Run.
//
// CloseInputs must be called exactly once per Run; calling it twice
// panics on the underlying channel close.
func (c *Coordinator) CloseInputs() {
	if c.cfg.PriorityBuffer != nil {
		// The drain goroutine in Run() owns close(c.stage1) and
		// exits once the buffer is closed and drained.
		c.cfg.PriorityBuffer.Close()
		return
	}
	close(c.stage1)
}

// runWithRetry executes fn with bounded retries on transient errors,
// threading a per-document key through to RetryAnalytics so
// success_after_retry can be computed. docKey may be empty (the
// analytics still updates the per-stage aggregates, but the
// success_after_retry attribution requires a key). Poison messages
// (ErrPoisonMessage) and ErrUnchanged are returned immediately
// without retry.
func (c *Coordinator) runWithRetry(ctx context.Context, stage, docKey string, fn func(context.Context) (any, error)) (any, error) {
	ctx, span := observability.StartSpan(ctx, "pipeline."+stage,
		observability.AttrStage.String(stage),
	)
	defer span.End()
	start := time.Now()
	defer func() {
		observability.ObserveStageDuration(stage, time.Since(start).Seconds())
	}()

	rec := c.cfg.RetryAnalytics
	stageTO := c.cfg.stageTimeout(stage)
	var lastErr error
	backoff := c.cfg.InitialBackoff
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		// Round-9 Task 8: apply the per-stage timeout to each
		// attempt. WithTimeout(parent, 0) is intentionally not
		// called so a zero-value config preserves prior
		// behaviour. Each retry attempt gets its OWN deadline
		// so an early-attempt deadline doesn't cap a healthy
		// later attempt.
		callCtx := ctx
		var cancel context.CancelFunc
		if stageTO > 0 {
			callCtx, cancel = context.WithTimeout(ctx, stageTO)
		}
		out, err := fn(callCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			if rec != nil {
				_ = rec.RecordAttempt(stage, docKey, RetryOutcomeSuccess, "")
			}
			return out, nil
		}
		if errors.Is(err, ErrUnchanged) {
			return nil, err
		}
		if errors.Is(err, ErrPoisonMessage) {
			observability.RecordError(span, err)
			if rec != nil {
				_ = rec.RecordAttempt(stage, docKey, RetryOutcomeFailed, err.Error())
			}
			return nil, err
		}
		lastErr = err
		if attempt < c.cfg.MaxAttempts {
			if rec != nil {
				_ = rec.RecordAttempt(stage, docKey, RetryOutcomeRetry, err.Error())
			}
		} else {
			observability.RecordError(span, err)
			if rec != nil {
				_ = rec.RecordAttempt(stage, docKey, RetryOutcomeFailed, err.Error())
			}
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
	c.recordSyncOutcome(evt, false)
	if c.cfg.OnDLQ != nil {
		c.cfg.OnDLQ(ctx, evt, err)
	}
}
