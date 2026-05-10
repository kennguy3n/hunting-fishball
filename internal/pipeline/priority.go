package pipeline

// priority.go — Round-6 Task 5.
//
// Multi-tenant pipelines occasionally need to favour latency-
// sensitive backfill (e.g. operator-triggered reindex of a single
// connector) over slow drip steady-state events. The
// PriorityClassifier returns a coarse priority bucket for each
// IngestEvent and PriorityBuffer multiplexes three FIFO buckets
// into a single downstream channel with anti-starvation.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Priority is the coarse priority bucket Stage 1 consults before
// admitting an event. The default classifier returns
// PriorityNormal; admin overrides bump or drop the bucket.
type Priority int

const (
	// PriorityHigh is reserved for operator-triggered reindex
	// (steady), targeted retries, and any event the admin
	// surface flagged as urgent.
	PriorityHigh Priority = iota
	// PriorityNormal is the default for steady-state Subscribe
	// events.
	PriorityNormal
	// PriorityLow is for backfill enumerations and best-effort
	// background work.
	PriorityLow
)

// String renders the bucket name (used in metrics labels).
func (p Priority) String() string {
	switch p {
	case PriorityHigh:
		return "high"
	case PriorityLow:
		return "low"
	default:
		return "normal"
	}
}

// ClassifierFunc is a closure-based PriorityClassifier
// implementation. The classifier receives the raw IngestEvent and
// returns the assigned bucket.
type ClassifierFunc func(IngestEvent) Priority

// Classify implements PriorityClassifier.
func (f ClassifierFunc) Classify(evt IngestEvent) Priority { return f(evt) }

// PriorityClassifier assigns a priority bucket to each event.
type PriorityClassifier interface {
	Classify(IngestEvent) Priority
}

// DefaultPriorityClassifier inspects (Kind, Source) plus any
// admin overrides and returns the assigned priority. Reindex
// events run at PriorityHigh, EventPurge / EventDocumentDeleted
// run at PriorityHigh (so deletes don't get stuck behind
// thousands of upserts), and steady Subscribe events run at
// PriorityNormal. Any source flagged in `Overrides` overrides the
// computed bucket.
type DefaultPriorityClassifier struct {
	// Overrides maps source_id → priority. Admins set entries here
	// (via the priority handler, future extension) to elevate or
	// demote a connector. Read-only after construction.
	Overrides map[string]Priority
	// SteadyKindIsHigh, when true, treats all Kind==EventSourceSync
	// events as PriorityHigh. Defaults to false.
	SteadyKindIsHigh bool
}

// Classify implements PriorityClassifier.
func (d DefaultPriorityClassifier) Classify(evt IngestEvent) Priority {
	if d.Overrides != nil {
		if p, ok := d.Overrides[evt.SourceID]; ok {
			return p
		}
	}
	switch evt.Kind {
	case EventReindex, EventDocumentDeleted, EventPurge:
		return PriorityHigh
	case EventDocumentChanged:
		if d.SteadyKindIsHigh {
			return PriorityHigh
		}
		return PriorityNormal
	default:
		return PriorityNormal
	}
}

// PriorityBuffer is a 3-channel multiplexer. Producers Push
// events tagged with a Priority; the buffer drains them to
// downstream() in priority order with a configurable
// anti-starvation cycle.
//
// Anti-starvation: after `LowAfter` consecutive non-low picks the
// buffer forces one low-priority pick. The same applies to
// `NormalAfter` for normal vs. high. Defaults: 4 high before
// one normal, 8 high+normal before one low. Tuneable per
// deployment.
type PriorityBuffer struct {
	cfg       PriorityBufferConfig
	high      chan IngestEvent
	normal    chan IngestEvent
	low       chan IngestEvent
	closed    atomic.Bool
	closeOnce sync.Once
	stats     priorityStats
}

// PriorityBufferConfig configures a PriorityBuffer.
type PriorityBufferConfig struct {
	// HighCapacity, NormalCapacity, LowCapacity size each bucket.
	// Defaults: 256 / 256 / 256.
	HighCapacity   int
	NormalCapacity int
	LowCapacity    int

	// NormalAfter is the number of high picks the buffer makes
	// before forcing one normal pick. Defaults to 4.
	NormalAfter int
	// LowAfter is the number of (high+normal) picks before
	// forcing one low pick. Defaults to 8.
	LowAfter int
}

type priorityStats struct {
	high   atomic.Int64
	normal atomic.Int64
	low    atomic.Int64
}

// NewPriorityBuffer constructs a PriorityBuffer.
func NewPriorityBuffer(cfg PriorityBufferConfig) *PriorityBuffer {
	if cfg.HighCapacity <= 0 {
		cfg.HighCapacity = 256
	}
	if cfg.NormalCapacity <= 0 {
		cfg.NormalCapacity = 256
	}
	if cfg.LowCapacity <= 0 {
		cfg.LowCapacity = 256
	}
	if cfg.NormalAfter <= 0 {
		cfg.NormalAfter = 4
	}
	if cfg.LowAfter <= 0 {
		cfg.LowAfter = 8
	}
	return &PriorityBuffer{
		cfg:    cfg,
		high:   make(chan IngestEvent, cfg.HighCapacity),
		normal: make(chan IngestEvent, cfg.NormalCapacity),
		low:    make(chan IngestEvent, cfg.LowCapacity),
	}
}

// ErrPriorityBufferClosed is returned by Push when the buffer is
// closed.
var ErrPriorityBufferClosed = errors.New("priority buffer closed")

// Push enqueues evt at the given priority. Blocks when the bucket
// is full; returns ctx.Err() if ctx is cancelled.
func (p *PriorityBuffer) Push(ctx context.Context, evt IngestEvent, prio Priority) error {
	if p.closed.Load() {
		return ErrPriorityBufferClosed
	}
	ch := p.bucket(prio)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- evt:
		return nil
	}
}

func (p *PriorityBuffer) bucket(prio Priority) chan IngestEvent {
	switch prio {
	case PriorityHigh:
		return p.high
	case PriorityLow:
		return p.low
	default:
		return p.normal
	}
}

// Close drains nothing and signals no more pushes are accepted.
// Drain reads any in-flight items and returns when all buckets are
// empty.
func (p *PriorityBuffer) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		close(p.high)
		close(p.normal)
		close(p.low)
	})
}

// Stats returns the cumulative pop counts per bucket. Useful for
// asserting starvation prevention in tests.
func (p *PriorityBuffer) Stats() (high, normal, low int64) {
	return p.stats.high.Load(), p.stats.normal.Load(), p.stats.low.Load()
}

// Drain consumes events from the buckets in strict priority order
// (high > normal > low) with anti-starvation counters, calling sink
// for each. Drain returns when all buckets are closed AND empty, or
// when ctx is cancelled.
//
// Implementation: we never use a multi-arm `select` over the three
// buckets directly because Go's select is randomised among ready
// cases — that would break strict ordering when both `high` and
// `normal` had items. Instead we try-receive in order, and only
// block (with a `select` over ctx.Done + the priority bucket) when
// every bucket appears empty.
func (p *PriorityBuffer) Drain(ctx context.Context, sink func(IngestEvent)) {
	consecHigh := 0
	consecNonLow := 0
	for {
		if ctx.Err() != nil {
			return
		}
		// Anti-starvation: force a low pick after `LowAfter` non-low
		// pops, force a normal pick after `NormalAfter` consecutive
		// high pops.
		if consecNonLow >= p.cfg.LowAfter {
			if v, ok := p.tryRecvOpen(p.low); ok {
				p.stats.low.Add(1)
				sink(v)
				consecHigh = 0
				consecNonLow = 0
				continue
			}
		}
		if consecHigh >= p.cfg.NormalAfter {
			if v, ok := p.tryRecvOpen(p.normal); ok {
				p.stats.normal.Add(1)
				sink(v)
				consecHigh = 0
				consecNonLow++
				continue
			}
		}
		if v, ok := p.tryRecvOpen(p.high); ok {
			p.stats.high.Add(1)
			sink(v)
			consecHigh++
			consecNonLow++
			continue
		}
		if v, ok := p.tryRecvOpen(p.normal); ok {
			p.stats.normal.Add(1)
			sink(v)
			consecHigh = 0
			consecNonLow++
			continue
		}
		if v, ok := p.tryRecvOpen(p.low); ok {
			p.stats.low.Add(1)
			sink(v)
			consecHigh = 0
			consecNonLow = 0
			continue
		}
		// All buckets returned EOF (no items + closed); we're done.
		if p.allClosed() {
			return
		}
		// Block on whichever bucket gets data first OR ctx fires.
		select {
		case <-ctx.Done():
			return
		case v, ok := <-p.high:
			if ok {
				p.stats.high.Add(1)
				sink(v)
				consecHigh++
				consecNonLow++
			}
		case v, ok := <-p.normal:
			if ok {
				p.stats.normal.Add(1)
				sink(v)
				consecHigh = 0
				consecNonLow++
			}
		case v, ok := <-p.low:
			if ok {
				p.stats.low.Add(1)
				sink(v)
				consecHigh = 0
				consecNonLow = 0
			}
		}
	}
}

// tryRecvOpen returns (event, true) only when ch has a buffered
// item. Returns (zero, false) when the bucket is empty OR closed —
// the caller separately checks closure via allClosed.
func (p *PriorityBuffer) tryRecvOpen(ch chan IngestEvent) (IngestEvent, bool) {
	if len(ch) == 0 {
		return IngestEvent{}, false
	}
	select {
	case v, ok := <-ch:
		if !ok {
			return IngestEvent{}, false
		}
		return v, true
	default:
		return IngestEvent{}, false
	}
}

// allClosed returns true when every bucket is closed AND empty.
// `closed` flips when Close() runs; once closed, len(ch) == 0
// indicates the channel has been fully drained.
func (p *PriorityBuffer) allClosed() bool {
	if !p.closed.Load() {
		return false
	}
	return len(p.high) == 0 && len(p.normal) == 0 && len(p.low) == 0
}
