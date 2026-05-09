package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// BackfillEmitter is the narrow contract the backfill orchestrator
// uses to publish per-document events. *Producer satisfies it; tests
// inject a fake.
type BackfillEmitter interface {
	EmitEvent(ctx context.Context, evt IngestEvent) error
}

// RateController paces backfill emissions so they don't starve the
// steady-state stream. The default implementation is a wall-clock
// ticker (TickerRate); the rate-limit task wires in a Redis token
// bucket from internal/admin.
type RateController interface {
	// Wait blocks until the next backfill emission slot is available
	// or ctx is cancelled. Returns ctx.Err() on cancellation.
	Wait(ctx context.Context) error
}

// BackfillConfig configures the backfill orchestrator.
type BackfillConfig struct {
	// Producer is the destination Kafka producer.
	Producer BackfillEmitter

	// Rate controls per-source backfill pacing. nil means "as fast as
	// the connector emits".
	Rate RateController

	// PageSize bounds the connector ListDocuments page. 0 → connector
	// default.
	PageSize int
}

// Backfill orchestrates the initial sync for a newly-connected
// source. The flow:
//
//  1. ListNamespaces() to discover the source's tenant of namespaces.
//  2. For each in-scope namespace, ListDocuments() — paginated.
//  3. For each document, Wait() on the rate controller, then publish
//     a SyncModeBackfill event onto Kafka.
//
// Backfill is a pull operation; the orchestrator never modifies the
// connector's state, so the same Backfill struct can run multiple
// concurrent backfills.
type Backfill struct {
	cfg BackfillConfig
}

// NewBackfill validates cfg and returns a Backfill.
func NewBackfill(cfg BackfillConfig) (*Backfill, error) {
	if cfg.Producer == nil {
		return nil, errors.New("backfill: nil Producer")
	}
	return &Backfill{cfg: cfg}, nil
}

// BackfillRequest is the input to Run. Scopes is the same shape used
// by admin.Source.Scopes — empty means "all namespaces".
type BackfillRequest struct {
	TenantID       string
	SourceID       string
	Connector      connector.SourceConnector
	Conn           connector.Connection
	Scopes         []string
	Connector_Type string //nolint:revive // existing name kept for clarity
}

// Validate returns an error when required fields are missing.
func (r BackfillRequest) Validate() error {
	if r.TenantID == "" || r.SourceID == "" {
		return errors.New("backfill: missing tenant/source")
	}
	if r.Connector == nil || r.Conn == nil {
		return errors.New("backfill: missing connector handle")
	}
	return nil
}

// BackfillResult summarises a Run.
type BackfillResult struct {
	Namespaces int
	Documents  int
	StartedAt  time.Time
	FinishedAt time.Time
}

// Run executes the backfill. Returns the count of namespaces and
// documents emitted plus the wall-clock window.
func (b *Backfill) Run(ctx context.Context, req BackfillRequest) (BackfillResult, error) {
	res := BackfillResult{StartedAt: time.Now().UTC()}
	if err := req.Validate(); err != nil {
		return res, err
	}

	namespaces, err := req.Connector.ListNamespaces(ctx, req.Conn)
	if err != nil {
		return res, fmt.Errorf("backfill: list namespaces: %w", err)
	}
	scopeAllow := buildScopeFilter(req.Scopes)

	for _, ns := range namespaces {
		if !scopeAllow(ns) {
			continue
		}
		res.Namespaces++
		if err := b.runNamespace(ctx, req, ns, &res); err != nil {
			return res, err
		}
	}

	res.FinishedAt = time.Now().UTC()
	return res, nil
}

func (b *Backfill) runNamespace(ctx context.Context, req BackfillRequest, ns connector.Namespace, res *BackfillResult) error {
	pageToken := ""
	for {
		opts := connector.ListOpts{PageSize: b.cfg.PageSize, PageToken: pageToken}
		it, err := req.Connector.ListDocuments(ctx, req.Conn, ns, opts)
		if err != nil {
			return fmt.Errorf("backfill: list documents: %w", err)
		}
		more, next, err := b.drainIterator(ctx, req, ns, it, res)
		_ = it.Close()
		if err != nil {
			return err
		}
		if !more || next == "" {
			return nil
		}
		pageToken = next
	}
}

// drainIterator pumps a single ListDocuments page into Kafka, calling
// Wait() between emits. Returns (hasMorePages, nextPageToken, err).
// Most connectors signal "more" via DocumentIterator.Err returning
// nil + an externally-tracked PageToken — the simple iterator
// contract here drives one page per call.
func (b *Backfill) drainIterator(ctx context.Context, req BackfillRequest, ns connector.Namespace, it connector.DocumentIterator, res *BackfillResult) (bool, string, error) {
	for it.Next(ctx) {
		if b.cfg.Rate != nil {
			if err := b.cfg.Rate.Wait(ctx); err != nil {
				return false, "", err
			}
		}
		ref := it.Doc()
		evt := IngestEvent{
			Kind:        EventDocumentChanged,
			TenantID:    req.TenantID,
			SourceID:    req.SourceID,
			DocumentID:  ref.ID,
			NamespaceID: ns.ID,
			SyncMode:    SyncModeBackfill,
		}
		if err := b.cfg.Producer.EmitEvent(ctx, evt); err != nil {
			return false, "", fmt.Errorf("backfill: emit: %w", err)
		}
		res.Documents++
	}
	if err := it.Err(); err != nil && !errors.Is(err, connector.ErrEndOfPage) {
		return false, "", fmt.Errorf("backfill: iterator: %w", err)
	}
	// The single-page-per-iterator pattern above doesn't expose a
	// next-page token through the DocumentIterator contract; concrete
	// connectors that need pagination can extend ListOpts. Returning
	// (false, "", nil) terminates the namespace cleanly.
	return false, "", nil
}

// buildScopeFilter returns a predicate that decides whether a
// namespace passes the scope list. Empty scopes admits everything;
// non-empty scopes match by namespace ID or by glob prefix
// (`drive-*`).
func buildScopeFilter(scopes []string) func(connector.Namespace) bool {
	if len(scopes) == 0 {
		return func(connector.Namespace) bool { return true }
	}
	exact := make(map[string]struct{}, len(scopes))
	prefixes := []string{}
	for _, s := range scopes {
		if strings.HasSuffix(s, "*") {
			prefixes = append(prefixes, strings.TrimSuffix(s, "*"))
			continue
		}
		exact[s] = struct{}{}
	}
	return func(ns connector.Namespace) bool {
		if _, ok := exact[ns.ID]; ok {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(ns.ID, p) {
				return true
			}
		}
		return false
	}
}

// TickerRate is a wall-clock RateController that emits at most
// `Per` events per `Window`. For a 10 docs/sec budget, use
// TickerRate{Per: 10, Window: time.Second}.
//
// The implementation is a simple time.Ticker — fine for unit tests
// and lower-volume sources; high-volume tenants should plug in the
// Redis token bucket from internal/admin/ratelimit.go.
type TickerRate struct {
	Per    int
	Window time.Duration

	tick *time.Ticker
}

// Start arms the ticker. Idempotent.
func (r *TickerRate) Start() {
	if r.tick != nil || r.Per <= 0 || r.Window <= 0 {
		return
	}
	r.tick = time.NewTicker(r.Window / time.Duration(r.Per))
}

// Stop releases the ticker resources.
func (r *TickerRate) Stop() {
	if r.tick != nil {
		r.tick.Stop()
		r.tick = nil
	}
}

// Wait blocks until the next tick or ctx cancellation.
func (r *TickerRate) Wait(ctx context.Context) error {
	if r.tick == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.tick.C:
		return nil
	}
}
