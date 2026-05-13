package admin

// health_summary_round19_probes.go — Round-19 Task 27.
//
// Four new MessageProbes that extend GET /v1/admin/health/summary
// with operationally-relevant signals beyond a binary up/down:
//
//   - stale_connectors    — sources that haven't synced in >24h
//   - dlq_growth          — DLQ growth rate over the last hour
//   - embedding_model     — current embedding-model availability
//   - tantivy_disk        — disk usage for the Tantivy index dir
//
// Each probe is self-contained: it owns its dependencies and
// emits a single line of message that the admin dashboard can
// render inline next to the status pill.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// StaleConnectorReader returns the number of sources that have
// not completed a sync within `staleAfter` of `now`.
type StaleConnectorReader interface {
	CountStale(ctx context.Context, staleAfter time.Duration) (int, error)
}

// StaleConnectorsProbe checks for sources that haven't synced
// recently. Degraded when at least one source is stale; unhealthy
// when more than `unhealthyAt` sources are stale.
type StaleConnectorsProbe struct {
	Reader      StaleConnectorReader
	StaleAfter  time.Duration
	UnhealthyAt int

	mu      sync.Mutex
	message string
}

// Name implements SummaryProbe.
func (p *StaleConnectorsProbe) Name() string { return "stale_connectors" }

// Check implements SummaryProbe.
func (p *StaleConnectorsProbe) Check(ctx context.Context) (SummaryStatus, error) {
	staleAfter := p.StaleAfter
	if staleAfter <= 0 {
		staleAfter = 24 * time.Hour
	}
	unhealthyAt := p.UnhealthyAt
	if unhealthyAt <= 0 {
		unhealthyAt = 5
	}
	n, err := p.Reader.CountStale(ctx, staleAfter)
	if err != nil {
		p.setMessage("")
		return SummaryStatusUnhealthy, err
	}
	switch {
	case n == 0:
		p.setMessage("")
		return SummaryStatusHealthy, nil
	case n >= unhealthyAt:
		p.setMessage(fmt.Sprintf("%d sources stale for >%s — investigate connector outage", n, staleAfter))
		return SummaryStatusUnhealthy, nil
	default:
		p.setMessage(fmt.Sprintf("%d sources stale for >%s", n, staleAfter))
		return SummaryStatusDegraded, nil
	}
}

// LastMessage implements MessageProbe.
func (p *StaleConnectorsProbe) LastMessage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.message
}

func (p *StaleConnectorsProbe) setMessage(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.message = s
}

// DLQGrowthReader returns the DLQ message count delta over the
// last `window`.
type DLQGrowthReader interface {
	GrowthOverWindow(ctx context.Context, window time.Duration) (int, error)
}

// DLQGrowthProbe degrades the summary when DLQ growth crosses a
// configurable threshold over the last hour.
type DLQGrowthProbe struct {
	Reader      DLQGrowthReader
	Window      time.Duration
	DegradeAt   int
	UnhealthyAt int

	mu      sync.Mutex
	message string
}

// Name implements SummaryProbe.
func (p *DLQGrowthProbe) Name() string { return "dlq_growth" }

// Check implements SummaryProbe.
func (p *DLQGrowthProbe) Check(ctx context.Context) (SummaryStatus, error) {
	window := p.Window
	if window <= 0 {
		window = time.Hour
	}
	degradeAt := p.DegradeAt
	if degradeAt <= 0 {
		degradeAt = 50
	}
	unhealthyAt := p.UnhealthyAt
	if unhealthyAt <= 0 {
		unhealthyAt = 500
	}
	growth, err := p.Reader.GrowthOverWindow(ctx, window)
	if err != nil {
		p.setMessage("")
		return SummaryStatusUnhealthy, err
	}
	switch {
	case growth >= unhealthyAt:
		p.setMessage(fmt.Sprintf("DLQ grew by %d in last %s — page on-call", growth, window))
		return SummaryStatusUnhealthy, nil
	case growth >= degradeAt:
		p.setMessage(fmt.Sprintf("DLQ grew by %d in last %s — inspect failure category mix", growth, window))
		return SummaryStatusDegraded, nil
	default:
		p.setMessage("")
		return SummaryStatusHealthy, nil
	}
}

// LastMessage implements MessageProbe.
func (p *DLQGrowthProbe) LastMessage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.message
}

func (p *DLQGrowthProbe) setMessage(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.message = s
}

// EmbeddingModelPinger returns nil when the configured embedding
// model is reachable. Implementations typically issue a tiny
// embedding RPC to the sidecar; the result vector is discarded.
type EmbeddingModelPinger interface {
	PingEmbeddingModel(ctx context.Context) error
}

// EmbeddingModelProbe degrades the summary when the embedding
// model is unreachable (any error from the pinger).
type EmbeddingModelProbe struct {
	Pinger EmbeddingModelPinger

	mu      sync.Mutex
	message string
}

// Name implements SummaryProbe.
func (p *EmbeddingModelProbe) Name() string { return "embedding_model" }

// Check implements SummaryProbe.
func (p *EmbeddingModelProbe) Check(ctx context.Context) (SummaryStatus, error) {
	if err := p.Pinger.PingEmbeddingModel(ctx); err != nil {
		p.setMessage("Embedding model sidecar unreachable — Stage 3 will queue")
		return SummaryStatusUnhealthy, err
	}
	p.setMessage("")
	return SummaryStatusHealthy, nil
}

// LastMessage implements MessageProbe.
func (p *EmbeddingModelProbe) LastMessage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.message
}

func (p *EmbeddingModelProbe) setMessage(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.message = s
}

// TantivyDiskReader returns the Tantivy index directory's bytes
// used and the disk capacity for the volume.
type TantivyDiskReader interface {
	IndexBytesUsed(ctx context.Context) (used int64, total int64, err error)
}

// TantivyDiskProbe degrades when used/total crosses warning;
// unhealthy when it crosses critical.
type TantivyDiskProbe struct {
	Reader   TantivyDiskReader
	Warn     float64 // 0..1
	Critical float64 // 0..1

	mu      sync.Mutex
	message string
}

// Name implements SummaryProbe.
func (p *TantivyDiskProbe) Name() string { return "tantivy_disk" }

// Check implements SummaryProbe.
func (p *TantivyDiskProbe) Check(ctx context.Context) (SummaryStatus, error) {
	warn := p.Warn
	if warn <= 0 || warn > 1 {
		warn = 0.7
	}
	crit := p.Critical
	if crit <= 0 || crit > 1 {
		crit = 0.9
	}
	used, total, err := p.Reader.IndexBytesUsed(ctx)
	if err != nil {
		p.setMessage("")
		return SummaryStatusUnhealthy, err
	}
	if total <= 0 {
		p.setMessage("")
		return SummaryStatusHealthy, nil
	}
	frac := float64(used) / float64(total)
	switch {
	case frac >= crit:
		p.setMessage(fmt.Sprintf("Tantivy disk at %.0f%% — page operator", frac*100))
		return SummaryStatusUnhealthy, nil
	case frac >= warn:
		p.setMessage(fmt.Sprintf("Tantivy disk at %.0f%% — plan compaction", frac*100))
		return SummaryStatusDegraded, nil
	default:
		p.setMessage("")
		return SummaryStatusHealthy, nil
	}
}

// LastMessage implements MessageProbe.
func (p *TantivyDiskProbe) LastMessage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.message
}

func (p *TantivyDiskProbe) setMessage(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.message = s
}
