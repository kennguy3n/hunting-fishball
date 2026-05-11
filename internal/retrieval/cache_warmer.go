// cache_warmer.go — Round-7 Task 9.
//
// CacheWarmer pre-populates the semantic cache for a list of
// (tenantID, query) tuples. It runs the retrieval pipeline for
// each entry through the configured Handler — the handler's
// existing post-retrieve `cache.Set` path writes the result back
// into Redis, so warming is just "issue the same call the
// production client would later make".
//
// The warmer is invoked from cmd/api at startup (background) and
// from POST /v1/admin/retrieval/warm-cache for operator-initiated
// warming.
package retrieval

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WarmTuple is a single (tenant_id, query) pair to warm. Optional
// fields control the request shape; defaults match a vanilla
// /v1/retrieve call.
type WarmTuple struct {
	TenantID    string
	Query       string
	TopK        int
	Channels    []string
	PrivacyMode string
}

// WarmResult is the per-tuple outcome.
type WarmResult struct {
	Tuple   WarmTuple
	Hits    int
	Latency time.Duration
	Err     error
}

// WarmSummary aggregates the WarmResult slice.
type WarmSummary struct {
	Total        int           `json:"total"`
	Succeeded    int           `json:"succeeded"`
	Failed       int           `json:"failed"`
	TotalLatency time.Duration `json:"total_latency"`
	Results      []WarmResult  `json:"results"`
}

// CacheWarmer runs Handler.RetrieveWithSnapshot for each tuple.
type CacheWarmer struct {
	handler  *Handler
	resolver PolicyResolver
}

// NewCacheWarmer validates inputs.
func NewCacheWarmer(handler *Handler, resolver PolicyResolver) (*CacheWarmer, error) {
	if handler == nil {
		return nil, errors.New("cache_warmer: nil handler")
	}
	if resolver == nil {
		return nil, errors.New("cache_warmer: nil resolver")
	}
	return &CacheWarmer{handler: handler, resolver: resolver}, nil
}

// Warm runs the handler for each tuple sequentially. Failures do
// not abort the batch — every tuple's outcome appears in the
// returned slice.
func (w *CacheWarmer) Warm(ctx context.Context, tuples []WarmTuple) WarmSummary {
	summary := WarmSummary{Total: len(tuples), Results: make([]WarmResult, 0, len(tuples))}
	if w == nil {
		return summary
	}
	for _, t := range tuples {
		start := time.Now()
		res := WarmResult{Tuple: t}
		if t.TenantID == "" || t.Query == "" {
			res.Err = errors.New("cache_warmer: missing tenant_id or query")
			summary.Failed++
			summary.Results = append(summary.Results, res)
			continue
		}
		snapshot, err := w.resolver.Resolve(ctx, t.TenantID, firstNonEmpty(t.Channels))
		if err != nil {
			res.Err = err
			summary.Failed++
			summary.Results = append(summary.Results, res)
			continue
		}
		req := RetrieveRequest{
			Query:       t.Query,
			TopK:        t.TopK,
			Channels:    append([]string{}, t.Channels...),
			PrivacyMode: t.PrivacyMode,
		}
		resp, err := w.handler.RetrieveWithSnapshot(ctx, t.TenantID, req, snapshot)
		res.Latency = time.Since(start)
		if err != nil {
			res.Err = err
			summary.Failed++
		} else {
			res.Hits = len(resp.Hits)
			summary.Succeeded++
		}
		summary.TotalLatency += res.Latency
		summary.Results = append(summary.Results, res)
	}
	return summary
}

// noopCacheWarmer returns an empty summary; convenient default
// when no warmer is wired.
type noopCacheWarmer struct{}

// NewNoopCacheWarmer returns a warmer that records nothing. Used
// in tests / environments without retrieval wiring.
func NewNoopCacheWarmer() *noopCacheWarmer { return &noopCacheWarmer{} }

// Warm implements the warmer surface as a no-op.
func (n *noopCacheWarmer) Warm(_ context.Context, _ []WarmTuple) WarmSummary { return WarmSummary{} }

// guard so the package always compiles even if cmd/api forgets to
// import sync. The unused-import lint would otherwise fire on
// tests that import the warmer without sync.
var _ sync.Locker = (*sync.Mutex)(nil)
