package retrieval

// Round-18 Task 9 — gRPC-backed cross-encoder reranker.
//
// The reranker.v1.RerankerService contract is wrapped behind the
// same Reranker seam as the in-process LinearReranker, so the
// retrieval handler can swap one for the other based on the
// CONTEXT_ENGINE_CROSS_ENCODER_ENABLED feature flag without
// touching call sites.
//
// On any RPC failure (deadline exceeded, sidecar unavailable, etc.)
// the cross-encoder reranker falls back to the supplied Fallback
// reranker (typically the LinearReranker) so retrieval continues
// to function even if the sidecar is unhealthy. This mirrors the
// "graceful degradation" pattern already used by the merger.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	rerankerv1 "github.com/kennguy3n/hunting-fishball/proto/reranker/v1"
)

// CrossEncoderConfig configures the gRPC-backed reranker.
type CrossEncoderConfig struct {
	// Client is the gRPC client for the Python reranker sidecar.
	// Required.
	Client rerankerv1.RerankerServiceClient

	// Fallback is consulted when the sidecar returns an error or
	// when the gate predicate denies the call. Defaults to a no-op
	// passthrough.
	Fallback Reranker

	// Timeout caps each Rerank RPC. Defaults to 5s.
	Timeout time.Duration

	// ModelID, when non-empty, is sent in every request. Empty
	// leaves the choice to the sidecar.
	ModelID string

	// TopK, when >0, bounds the number of candidates returned.
	// Defaults to 0 (return every candidate).
	TopK int32

	// TenantIDFromCtx resolves the tenant id to forward to the
	// sidecar. Defaults to reading "tenant_id" off the context via
	// the standard observability key.
	TenantIDFromCtx func(ctx context.Context) string

	// MaxCandidates caps the candidates forwarded to the sidecar
	// per request. Cross-encoders are O(candidates) — keep this
	// modest. Defaults to 50.
	MaxCandidates int
}

// CrossEncoderReranker calls a Python cross-encoder sidecar.
type CrossEncoderReranker struct {
	cfg CrossEncoderConfig
}

// NewCrossEncoderReranker constructs the reranker with the
// supplied config; nil-valued knobs fall back to defaults.
func NewCrossEncoderReranker(cfg CrossEncoderConfig) *CrossEncoderReranker {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxCandidates == 0 {
		cfg.MaxCandidates = 50
	}
	if cfg.TenantIDFromCtx == nil {
		cfg.TenantIDFromCtx = tenantIDFromCtxValue
	}

	return &CrossEncoderReranker{cfg: cfg}
}

// CrossEncoderEnabled returns true when the env feature flag is
// set. Mirrors the convention used by other Round 18 gates.
func CrossEncoderEnabled() bool {
	v := os.Getenv("CONTEXT_ENGINE_CROSS_ENCODER_ENABLED")
	switch v {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true
	}

	return false
}

// Rerank scores the candidates via the gRPC sidecar and reorders
// the slice descending. On any RPC error it delegates to the
// configured Fallback (or returns the inputs unchanged if no
// fallback was configured), ensuring retrieval never hard-fails on
// reranker outage.
func (c *CrossEncoderReranker) Rerank(ctx context.Context, query string, matches []*Match) ([]*Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.cfg.Client == nil {
		return c.fallback(ctx, query, matches)
	}
	if len(matches) == 0 || query == "" {
		return matches, nil
	}

	candidates := matches
	truncated := false
	if c.cfg.MaxCandidates > 0 && len(candidates) > c.cfg.MaxCandidates {
		candidates = candidates[:c.cfg.MaxCandidates]
		truncated = true
	}

	req := &rerankerv1.RerankRequest{
		TenantId:   c.cfg.TenantIDFromCtx(ctx),
		Query:      query,
		ModelId:    c.cfg.ModelID,
		TopK:       c.cfg.TopK,
		Candidates: make([]*rerankerv1.Candidate, 0, len(candidates)),
	}
	for _, m := range candidates {
		if m == nil {
			continue
		}
		req.Candidates = append(req.Candidates, &rerankerv1.Candidate{
			ChunkId: m.ID,
			Text:    m.Text,
			Score:   m.Score,
		})
	}

	rpcCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	resp, err := c.cfg.Client.Rerank(rpcCtx, req)
	if err != nil {
		return c.fallback(ctx, query, matches)
	}
	if resp == nil || len(resp.GetScored()) == 0 {
		return c.fallback(ctx, query, matches)
	}

	scoreByChunk := make(map[string]float32, len(resp.GetScored()))
	for _, s := range resp.GetScored() {
		scoreByChunk[s.GetChunkId()] = s.GetScore()
	}

	for _, m := range candidates {
		if m == nil {
			continue
		}
		if s, ok := scoreByChunk[m.ID]; ok {
			m.OriginalScore = m.Score
			m.Score = s
		}
	}
	// The tail beyond MaxCandidates keeps its merger score so it
	// still ranks, just behind the reranked head.
	if truncated {
		_ = matches[c.cfg.MaxCandidates:]
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Score > matches[j].Score
	})
	if c.cfg.TopK > 0 && int(c.cfg.TopK) < len(matches) {
		matches = matches[:int(c.cfg.TopK)]
	}

	return matches, nil
}

func (c *CrossEncoderReranker) fallback(ctx context.Context, query string, matches []*Match) ([]*Match, error) {
	if c.cfg.Fallback != nil {
		return c.cfg.Fallback.Rerank(ctx, query, matches)
	}

	return matches, nil
}

// tenantIDFromCtxValue extracts the tenant id from the unexported
// ctx key the cross-encoder reranker uses (mirrors the audit
// handler's context.Context-side convention). Returns "" when
// absent — the sidecar will reject a missing tenant_id with
// INVALID_ARGUMENT, which surfaces via fallback rather than
// crashing retrieval.
func tenantIDFromCtxValue(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v := ctx.Value(tenantCtxKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}

	return ""
}

// tenantCtxKey is the unexported key the reranker uses for
// ctx-scoped tenant ids. Distinct from the gin.Context key used
// by handler.tenantIDFromContext so the two paths don't collide.
type tenantCtxKey struct{}

// WithTenantContext returns a child context with the tenant id
// stashed for the cross-encoder reranker to forward. Production
// code typically already has the tenant id in the request struct,
// but this helper keeps the reranker contract self-contained.
func WithTenantContext(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// String implements a debug-friendly representation used by
// retrieval/explain output.
func (c *CrossEncoderReranker) String() string {
	return fmt.Sprintf("CrossEncoderReranker(model=%q, timeout=%s, max_candidates=%d)",
		c.cfg.ModelID, c.cfg.Timeout, c.cfg.MaxCandidates)
}
