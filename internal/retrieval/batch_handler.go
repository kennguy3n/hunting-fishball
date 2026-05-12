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

	"github.com/kennguy3n/hunting-fishball/internal/observability"
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
	// TraceID is the parent span trace id when distributed
	// tracing is active. Round-13 Task 3: operators correlate the
	// batch parent span with the N child sub-request spans by
	// pasting this id into Jaeger / Tempo.
	TraceID string `json:"trace_id,omitempty"`
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
		c.JSON(http.StatusBadRequest, BuildPayloadErrorBody(err))
		return
	}
	// Round-14 Task 5: enforce per-request schema validation
	// before fan-out. The 413 length check stays separate from
	// schema validation so existing callers that special-case
	// "batch too large" keep their semantics.
	if len(req.Requests) > MaxBatchSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "batch too large", "max": MaxBatchSize})
		return
	}
	if verr := ValidateBatchRequest(&req); verr != nil {
		c.JSON(http.StatusBadRequest, BuildPayloadErrorBody(verr))
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
//
// Round-13 Task 3: the entire batch is wrapped in a parent span
// ("retrieval.batch"). Each runOne call inherits the parent's
// context so its own per-request spans (e.g. retrieval.vector,
// retrieval.bm25) attach as descendants of the batch parent.
// The parent trace id is echoed back to the caller in
// BatchResponse.TraceID so operators can correlate the parent
// + children in a single Jaeger / Tempo view.
func (h *Handler) RunBatch(ctx context.Context, tenantID string, req BatchRequest) BatchResponse {
	parallel := req.MaxParallel
	if parallel <= 0 {
		parallel = DefaultBatchMaxParallel
	}
	if parallel > len(req.Requests) {
		parallel = len(req.Requests)
	}
	ctx, span := observability.StartSpan(ctx, "retrieval.batch",
		observability.AttrTenantID.String(tenantID),
	)
	span.SetAttributes(observability.AttrHitCount.Int(len(req.Requests)))
	defer span.End()
	results := make([]BatchResultItem, len(req.Requests))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range req.Requests {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			subCtx, subSpan := observability.StartSpan(ctx, "retrieval.batch.sub",
				observability.AttrTenantID.String(tenantID),
			)
			results[idx] = h.runOne(subCtx, tenantID, idx, req.Requests[idx])
			subSpan.End()
		}(i)
	}
	wg.Wait()
	return BatchResponse{Results: results, TraceID: observability.TraceID(ctx)}
}

func (h *Handler) runOne(ctx context.Context, tenantID string, index int, sub RetrieveRequest) BatchResultItem {
	if sub.Query == "" {
		return BatchResultItem{Index: index, Error: "query is required"}
	}
	// Round-14 Task 18: count every sub-request that survived
	// schema validation so the SlowQueryRateHigh alert's
	// denominator includes batch traffic.
	observability.RetrievalRequestsTotal.Inc()
	// Round-11 Task 9: tag every batch sub-request so the query
	// analytics recorder can distinguish batch sub-requests from
	// organic /v1/retrieve traffic. The field is internal (json:"-")
	// so the wire payload remains identical.
	sub.Source = QueryAnalyticsSourceBatch
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
	//
	// Analytics: RetrieveWithSnapshotCached records the query
	// analytics event itself (Round-11 Devin Review fix), reading
	// sub.Source above and resolving topK/DefaultTopK consistently
	// with the request that actually ran — the batch handler no
	// longer records explicitly, which used to mis-report topK as
	// `len(resp.Hits)` when the caller omitted top_k.
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
