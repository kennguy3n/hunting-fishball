// batch_handler.go — Phase 8 / Task 12 bulk retrieve.
//
// Dashboard widgets and chat clients often need to populate a panel
// from multiple, independent retrieve queries (e.g. "open issues",
// "recent docs", "recent slack threads"). Instead of forcing N
// round-trips, POST /v1/retrieve/batch accepts a slice of
// RetrieveRequest values and fans them out concurrently with a
// configurable max-parallelism cap.
//
// Each sub-request's policy is resolved independently so a tenant's
// per-channel privacy mode is honoured exactly as it would be on the
// single-request endpoint. A failure in one sub-request is surfaced
// as an error in that slot — the remaining slots still complete and
// the HTTP envelope is always 200.
package retrieval

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// DefaultBatchMaxParallel caps the per-batch fan-out width. The cap
// protects shared backends (Qdrant, embedder gRPC) from one client
// burning every connection in the pool.
const DefaultBatchMaxParallel = 8

// MaxBatchSize bounds the number of sub-requests in one batch. Larger
// payloads are rejected with 413 to keep tail-latency bounded.
const MaxBatchSize = 32

// BatchRequest is the JSON shape of POST /v1/retrieve/batch.
type BatchRequest struct {
	Requests    []RetrieveRequest `json:"requests" binding:"required"`
	MaxParallel int               `json:"max_parallel,omitempty"`
}

// BatchResultItem is the per-sub-request result envelope. Exactly one
// of `response` / `error` is populated for each item.
type BatchResultItem struct {
	Index    int               `json:"index"`
	Response *RetrieveResponse `json:"response,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// BatchResponse is the JSON shape returned by /v1/retrieve/batch.
type BatchResponse struct {
	Results []BatchResultItem `json:"results"`
}

// RegisterBatch mounts POST /v1/retrieve/batch on rg.
func (h *Handler) RegisterBatch(rg *gin.RouterGroup) {
	rg.POST("/v1/retrieve/batch", h.retrieveBatch)
}

func (h *Handler) retrieveBatch(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req BatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(req.Requests) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "requests is required"})
		return
	}
	if len(req.Requests) > MaxBatchSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "batch too large", "max": MaxBatchSize})
		return
	}
	// Round-9 Task 7: explain mode flows through the batch fan-out
	// just like the single-request /v1/retrieve endpoint. The
	// auth check happens once at the gin layer; sub-requests that
	// the caller asked to explain are stripped of the flag when
	// the caller is not authorised so the downstream pipeline
	// can't leak debug signals.
	explainAuthorised := IsExplainAuthorized(c, h.cfg.ExplainEnvEnabled)
	if !explainAuthorised {
		for i := range req.Requests {
			req.Requests[i].Explain = false
		}
	}
	resp := h.RunBatch(c.Request.Context(), tenantID, req)
	c.JSON(http.StatusOK, resp)
}

// RunBatch is the production entry point exposed for tests and for
// in-process callers (e.g. internal admin tools). It fans out every
// sub-request concurrently with the configured parallelism cap and
// returns one BatchResultItem per input.
func (h *Handler) RunBatch(ctx context.Context, tenantID string, req BatchRequest) BatchResponse {
	parallel := req.MaxParallel
	if parallel <= 0 {
		parallel = DefaultBatchMaxParallel
	}
	if parallel > len(req.Requests) {
		parallel = len(req.Requests)
	}
	results := make([]BatchResultItem, len(req.Requests))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range req.Requests {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = h.runOne(ctx, tenantID, idx, req.Requests[idx])
		}(i)
	}
	wg.Wait()
	return BatchResponse{Results: results}
}

func (h *Handler) runOne(ctx context.Context, tenantID string, index int, sub RetrieveRequest) BatchResultItem {
	if sub.Query == "" {
		return BatchResultItem{Index: index, Error: "query is required"}
	}
	channelID := firstNonEmpty(sub.Channels)
	snapshot, err := h.cfg.PolicyResolver.Resolve(ctx, tenantID, channelID)
	if err != nil {
		return BatchResultItem{Index: index, Error: "policy resolve failed"}
	}
	// Batch sub-requests run against the LIVE policy snapshot just
	// resolved above, so they share the semantic cache with the
	// single-request /v1/retrieve endpoint. Pre-fix this called
	// RetrieveWithSnapshot which intentionally skips the cache for
	// the Phase 4 simulator (draft snapshots) — a hot dashboard
	// query was paying the full fan-out cost on every batch slot.
	// RetrieveWithSnapshotCached re-uses the live snapshot's cache
	// slot so a sub-ms cache hit is possible per slot.
	resp, rerr := h.RetrieveWithSnapshotCached(ctx, tenantID, sub, snapshot)
	if rerr != nil {
		if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
			return BatchResultItem{Index: index, Error: rerr.Error()}
		}
		return BatchResultItem{Index: index, Error: rerr.Error()}
	}
	if resp.Policy.PrivacyMode == "" {
		resp.Policy.PrivacyMode = string(policy.PrivacyModeNoAI)
	}
	return BatchResultItem{Index: index, Response: &resp}
}
