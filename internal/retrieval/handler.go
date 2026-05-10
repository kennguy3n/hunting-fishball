// Package retrieval serves the Phase 1 / Phase 3 retrieval API: a
// single POST /v1/retrieve endpoint that fans out across the vector,
// BM25, graph, and memory backends, applies tenant policy, and
// returns ranked results with provenance and privacy_label.
//
// Tenant isolation is enforced the same way the audit handler does it
// — tenant_id MUST come from the Gin request context (populated by
// auth middleware) and is never read from the request body or query
// params. Each backend client also enforces tenant scoping at the
// storage boundary (ErrMissingTenantScope), so even a misconfigured
// handler can't leak across tenants.
package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// VectorStore is the narrow contract the retrieval handler needs from
// the underlying vector database. *storage.QdrantClient satisfies it.
type VectorStore interface {
	Search(ctx context.Context, tenantID string, vector []float32, opts storage.SearchOpts) ([]storage.QdrantHit, error)
}

// Embedder produces a single-query embedding. The retrieval handler
// uses it to convert the user's text query into a vector.
type Embedder interface {
	EmbedQuery(ctx context.Context, tenantID, query string) ([]float32, error)
}

// BM25Backend is the lexical retrieval seam. *storage.TantivyClient
// satisfies it via the small adapter in storage_adapter.go.
type BM25Backend interface {
	Search(ctx context.Context, tenantID, query string, k int) ([]*Match, error)
}

// GraphBackend is the graph traversal seam. *storage.FalkorDBClient
// satisfies it via the storage_adapter.go projection.
type GraphBackend interface {
	Traverse(ctx context.Context, tenantID, query string, k int) ([]*Match, error)
}

// MemoryBackend is the persistent memory seam (Mem0). The cmd/api
// wiring constructs a tiny gRPC adapter that satisfies it.
type MemoryBackend interface {
	Search(ctx context.Context, tenantID, query string, k int) ([]*Match, error)
}

// Cache is the semantic cache seam. *storage.SemanticCache satisfies
// it.
type Cache interface {
	Get(ctx context.Context, tenantID, channelID string, queryEmbedding []float32, scopeHash string) (*storage.CachedResult, error)
	Set(ctx context.Context, tenantID, channelID string, queryEmbedding []float32, scopeHash string, result *storage.CachedResult, ttl time.Duration) error
}

// HandlerConfig configures the retrieval Handler. VectorStore and
// Embedder are required (Phase 1 contract); BM25, Graph, Memory,
// Cache, Merger, Reranker, and PolicyFilter are optional Phase 3
// upgrades — when nil, the handler degrades to the Phase 1 single-
// stream behaviour.
type HandlerConfig struct {
	VectorStore VectorStore
	Embedder    Embedder

	// Phase 3 backends. Any of these may be nil; missing backends are
	// skipped during fan-out and surface in `policy.applied` as the
	// backends that *did* contribute.
	BM25         BM25Backend
	Graph        GraphBackend
	Memory       MemoryBackend
	Cache        Cache
	Merger       *Merger
	Reranker     Reranker
	PolicyFilter *PolicyFilter

	// Diversifier optionally re-orders post-rerank results to
	// favour topical diversity (Round-6 Task 1, MMR). When nil
	// the handler installs `*MMRDiversifier` with the default
	// token-Jaccard similarity. Callers opt in per-request via
	// `RetrieveRequest.Diversity`; the default lambda of 0.0
	// keeps the legacy passthrough behaviour intact.
	Diversifier Diversifier

	// QueryExpander optionally expands the user's query with
	// per-tenant synonyms before the BM25 / memory fan-out
	// (Round-6 Task 4). When nil the handler skips expansion.
	QueryExpander QueryExpander

	// ChunkACL optionally enforces per-chunk ACL tags after the
	// snapshot ACL has been applied (Round-6 Task 6). When nil
	// the handler runs no chunk-level filter.
	ChunkACL ChunkACLEvaluator

	// ABTests is the optional retrieval A/B-testing surface
	// (Round-6 Task 10). When nil the handler runs the control
	// configuration on every request. The concrete type lives in
	// ab_test.go.
	ABTests ABTestRouter

	// DefaultTopK is the default top_k when the request doesn't set
	// one. Defaults to 10.
	DefaultTopK int

	// MaxTopK caps the user-supplied top_k. Defaults to 100.
	MaxTopK int

	// PerBackendDeadline caps each fan-out backend call. Defaults to
	// 250ms (well below the API's overall budget per
	// ARCHITECTURE.md §4.2).
	PerBackendDeadline time.Duration

	// CacheTTL is the TTL for newly-written cache entries. Defaults
	// to 5 minutes.
	CacheTTL time.Duration

	// DefaultPrivacyMode is the channel privacy mode used when the
	// request doesn't supply one. Defaults to "internal".
	DefaultPrivacyMode string

	// PolicyResolver loads the (tenant, channel) policy snapshot
	// before fan-out. nil → permissive default (Phase 1/3
	// privacy-label filter only).
	PolicyResolver PolicyResolver

	// ShardVersionLookup returns the freshest on-device shard
	// version for the request scope (tenant, channel,
	// privacy_mode). Optional — when nil, the handler always
	// reports `local_shard_version: 0`, which forces
	// prefer_local=false from the device-first policy. Phase 6 /
	// Task 15.
	ShardVersionLookup ShardVersionLookup

	// ExplainEnvEnabled, when true, opts the deployment in to the
	// per-hit `explain` block for non-admin callers as well. The
	// production wiring sets this from the
	// CONTEXT_ENGINE_EXPLAIN_ENABLED env var so support engineers
	// can debug reranker decisions without needing the admin RBAC
	// role. Round-5 Task 12.
	ExplainEnvEnabled bool
}

// ShardVersionLookup is the narrow port the retrieval handler uses
// to learn the freshest shard version for a request scope. The
// production wiring delegates to internal/shard.Repository's
// LatestVersion; tests use an in-memory fake.
type ShardVersionLookup interface {
	LatestShardVersion(ctx context.Context, tenantID, channelID, privacyMode string) (int64, error)
}

// Handler serves the retrieval HTTP API.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler validates cfg and returns a Handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.VectorStore == nil {
		return nil, errors.New("retrieval: nil VectorStore")
	}
	if cfg.Embedder == nil {
		return nil, errors.New("retrieval: nil Embedder")
	}
	if cfg.DefaultTopK == 0 {
		cfg.DefaultTopK = 10
	}
	if cfg.MaxTopK == 0 {
		cfg.MaxTopK = 100
	}
	if cfg.PerBackendDeadline == 0 {
		cfg.PerBackendDeadline = 250 * time.Millisecond
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.DefaultPrivacyMode == "" {
		cfg.DefaultPrivacyMode = "internal"
	}
	if cfg.Merger == nil {
		cfg.Merger = NewMerger(MergerConfig{})
	}
	if cfg.Reranker == nil {
		cfg.Reranker = NewLinearReranker(LinearRerankerConfig{})
	}
	if cfg.PolicyFilter == nil {
		cfg.PolicyFilter = NewPolicyFilter(PolicyConfig{})
	}
	if cfg.PolicyResolver == nil {
		cfg.PolicyResolver = noopPolicyResolver{}
	}
	if cfg.Diversifier == nil {
		cfg.Diversifier = NewMMRDiversifier(nil)
	}

	return &Handler{cfg: cfg}, nil
}

// Register mounts the retrieval endpoints on rg. Routes:
//
//	POST /v1/retrieve
//	POST /v1/retrieve/batch
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/retrieve", h.retrieve)
	h.RegisterBatch(rg)
}

// RetrieveRequest is the JSON shape of POST /v1/retrieve.
type RetrieveRequest struct {
	Query     string   `json:"query" binding:"required"`
	TopK      int      `json:"top_k"`
	Sources   []string `json:"sources,omitempty"`
	Channels  []string `json:"channels,omitempty"`
	Documents []string `json:"document_ids,omitempty"`

	// PrivacyMode is the channel's declared privacy mode. The policy
	// filter drops chunks whose privacy_label exceeds this. Defaults
	// to HandlerConfig.DefaultPrivacyMode when empty.
	PrivacyMode string `json:"privacy_mode,omitempty"`

	// SkillID is the calling skill ("summarizer", "qa-bot", ...).
	// Used by the recipient policy to gate which downstream
	// consumers may receive the result. Empty disables the
	// recipient gate (legacy callers).
	SkillID string `json:"skill_id,omitempty"`

	// DeviceTier is the requesting device's effective tier
	// (low/mid/high). Drives the on-device-first policy: when
	// supplied as Mid or High and the channel + privacy mode
	// permit it, the response carries `prefer_local: true` so the
	// client can choose to serve from the on-device shard. Empty
	// means "unknown", which collapses to remote-only behaviour.
	DeviceTier string `json:"device_tier,omitempty"`

	// Explain, when true and the caller is authorised (admin
	// RBAC role OR the deployment-wide CONTEXT_ENGINE_EXPLAIN_ENABLED
	// feature flag), populates a per-hit `explain` block exposing
	// the raw signal scores and policy decision. Off by default.
	Explain bool `json:"explain,omitempty"`

	// Diversity is the MMR `lambda` parameter in [0, 1]. 0
	// (default) keeps the legacy ordering, 1 maximises
	// inter-result dissimilarity, intermediate values blend
	// relevance with diversity. Round-6 Task 1.
	Diversity float32 `json:"diversity,omitempty"`

	// ExperimentName, when set, opts the request into a named
	// retrieval A/B test (Round-6 Task 10). The handler resolves
	// the experiment from the configured `ABTestStore`, computes
	// the bucket from `ExperimentBucketKey || tenant_id`, and
	// applies either the control or the variant retrieval config.
	ExperimentName      string `json:"experiment_name,omitempty"`
	ExperimentBucketKey string `json:"experiment_bucket_key,omitempty"`
}

// RetrieveHit is one entry in the response.
type RetrieveHit struct {
	ID           string         `json:"id"`
	Score        float32        `json:"score"`
	TenantID     string         `json:"tenant_id"`
	SourceID     string         `json:"source_id,omitempty"`
	DocumentID   string         `json:"document_id,omitempty"`
	BlockID      string         `json:"block_id,omitempty"`
	Title        string         `json:"title,omitempty"`
	URI          string         `json:"uri,omitempty"`
	PrivacyLabel string         `json:"privacy_label,omitempty"`
	Text         string         `json:"text,omitempty"`
	Connector    string         `json:"connector,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	Sources      []string       `json:"sources,omitempty"`

	// PrivacyStrip is the structured privacy disclosure clients
	// render for the user. The handler builds it from the chunk's
	// privacy_label and the resolved PolicySnapshot. Phase 4
	// addition.
	PrivacyStrip PrivacyStrip `json:"privacy_strip"`

	// Explain is the per-hit debug projection populated when the
	// request opted in via `"explain": true` AND the caller is
	// authorised (admin RBAC role OR ExplainEnvVar set to "1" /
	// "true"). Nil otherwise. See explain.go for the field
	// semantics.
	Explain *RetrievalExplain `json:"explain,omitempty"`
}

// RetrievePolicy summarises the policy decisions made during fan-out.
type RetrievePolicy struct {
	// Applied lists the backend names that contributed at least one
	// match (e.g. ["vector", "bm25"]).
	Applied []string `json:"applied,omitempty"`
	// Degraded names backends that timed out or failed within the
	// per-backend deadline. The handler still returns the partial
	// result; clients can render a "degraded" badge.
	Degraded []string `json:"degraded,omitempty"`
	// BlockedCount counts chunks dropped by the policy filter.
	BlockedCount int `json:"blocked_count"`
	// CacheHit is true when the response was served from the
	// semantic cache.
	CacheHit bool `json:"cache_hit"`
	// PrivacyMode is the channel privacy mode the request resolved
	// to (echoed for the client UI).
	PrivacyMode string `json:"privacy_mode,omitempty"`

	// ExperimentName is set when the request was routed through
	// an A/B test (Round-6 Task 10). Empty otherwise.
	ExperimentName string `json:"experiment_name,omitempty"`
	// ExperimentArm is the arm the request was routed to
	// ("control" or "variant"). Empty when no experiment applied.
	ExperimentArm string `json:"experiment_arm,omitempty"`
}

// RetrieveTimings reports the wall-clock duration of every stage
// in milliseconds so operators can identify the long pole. The
// fan-out backends (vector / bm25 / graph / memory) run in
// parallel; their values are independent. Merge and Rerank run
// serially after fan-out completes.
type RetrieveTimings struct {
	VectorMs int64 `json:"vector_ms"`
	BM25Ms   int64 `json:"bm25_ms"`
	GraphMs  int64 `json:"graph_ms"`
	MemoryMs int64 `json:"memory_ms"`
	MergeMs  int64 `json:"merge_ms"`
	RerankMs int64 `json:"rerank_ms"`
}

// RetrieveResponse is the JSON envelope for POST /v1/retrieve.
type RetrieveResponse struct {
	Hits    []RetrieveHit   `json:"hits"`
	Policy  RetrievePolicy  `json:"policy"`
	Timings RetrieveTimings `json:"timings"`
	// TraceID echoes the OpenTelemetry trace_id of the retrieval
	// span so clients can correlate slow requests with backend
	// traces. Empty when no tracer is active. See
	// docs/ARCHITECTURE.md §4.1.
	TraceID string `json:"trace_id,omitempty"`

	// PreferLocal is the on-device-first hint. When true the client
	// SHOULD serve the query from its local shard at
	// `LocalShardVersion` and only fall back to the response Hits if
	// the local coverage is insufficient (see
	// `docs/contracts/local-first-retrieval.md` for the decision
	// tree). Phase 6 / Task 15.
	PreferLocal bool `json:"prefer_local"`

	// LocalShardVersion is the freshest shard version the server
	// observed for the request scope. Echoed even when PreferLocal
	// is false so a client that has just upgraded its tier can
	// see whether a shard is already on the device.
	LocalShardVersion int64 `json:"local_shard_version,omitempty"`

	// PreferLocalReason is a stable label explaining the
	// PreferLocal decision: one of
	// `device_tier_too_low|channel_disallowed|privacy_blocks_local|no_local_shard|prefer_local`.
	PreferLocalReason string `json:"prefer_local_reason,omitempty"`
}

func (h *Handler) retrieve(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}

	var req RetrieveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})

		return
	}
	if req.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})

		return
	}
	topK := req.TopK
	if topK == 0 {
		topK = h.cfg.DefaultTopK
	}
	if topK < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "top_k must be >= 0"})

		return
	}
	if topK > h.cfg.MaxTopK {
		topK = h.cfg.MaxTopK
	}
	privacyMode := req.PrivacyMode
	if privacyMode == "" {
		privacyMode = h.cfg.DefaultPrivacyMode
	}

	channelIDForPolicy := firstNonEmpty(req.Channels)
	snapshot, perr := h.cfg.PolicyResolver.Resolve(c.Request.Context(), tenantID, channelIDForPolicy)
	if perr != nil {
		// Fail closed: a resolver failure must not silently widen
		// the policy. Return zero hits with the safest privacy mode.
		c.JSON(http.StatusServiceUnavailable, RetrieveResponse{
			Hits: []RetrieveHit{},
			Policy: RetrievePolicy{
				PrivacyMode:  string(policy.PrivacyModeNoAI),
				BlockedCount: 0,
			},
		})
		return
	}
	// Privacy mode is admin-controlled. When the resolver populated
	// EffectiveMode it is the source of truth — even when the
	// effective mode is more permissive than the caller's request.
	// The request-supplied PrivacyMode (or HandlerConfig
	// .DefaultPrivacyMode) is only a fallback for environments
	// without a resolver wired in (the noop resolver leaves
	// EffectiveMode empty).
	if snapshot.EffectiveMode != "" {
		privacyMode = string(snapshot.EffectiveMode)
	}

	vec, err := h.cfg.Embedder.EmbedQuery(c.Request.Context(), tenantID, req.Query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "embed failed"})

		return
	}

	channelID := firstNonEmpty(req.Channels)
	scopeHash := scopeHashFor(req, topK, privacyMode)

	// Cache short-circuit. A cache hit serves the response without
	// any fan-out (ARCHITECTURE.md §4.4). On error the handler
	// degrades to a fan-out — a stale cache should never take
	// retrieval offline.
	//
	// The Phase 4 privacy-label PolicyFilter, ACL, and recipient
	// gates are re-evaluated on the cache-hit path because the
	// cache TTL is minutes-long: a policy change between write and
	// read must NOT serve content the new policy denies. Note that
	// scopeHashFor already keys on the resolved privacyMode, so a
	// post-resolver mode change writes to a fresh slot — but until
	// that slot is populated, the prior slot can still hand out
	// matches from the more permissive mode. Re-running the gates
	// here closes that window without having to invalidate.
	if h.cfg.Cache != nil {
		cached, cerr := h.cfg.Cache.Get(c.Request.Context(), tenantID, channelID, vec, scopeHash)
		if cerr == nil && cached != nil {
			filtered, blockedByPrivacy := filterCachedByPrivacyMode(cached, h.cfg.PolicyFilter, privacyMode)
			filtered, blockedByACL, blockedByRecipient := filterCachedBySnapshot(filtered, snapshot, req.SkillID)
			resp := responseFromCache(filtered, privacyMode, topK)
			resp.Policy.BlockedCount = blockedByPrivacy + blockedByACL + blockedByRecipient
			// Cached hits carry the same PrivacyStrip enrichment as
			// fresh hits — caching only the strip would
			// double-count if the live policy changes between
			// writes. We rebuild the strip from the resolved
			// snapshot at serve time.
			for i := range resp.Hits {
				resp.Hits[i].PrivacyStrip = BuildPrivacyStrip(matchFromCachedHit(resp.Hits[i]), snapshot)
			}
			h.applyDeviceFirst(c.Request.Context(), tenantID, channelID, privacyMode, req.DeviceTier, snapshot, &resp)
			c.JSON(http.StatusOK, resp)

			return
		}
	}

	// Round-6 Task 4: optional synonym expansion before fan-out.
	// The expanded query is what BM25 / memory see; the embedding
	// path stays on the original (already vectorised above).
	expandedQuery := req.Query
	if h.cfg.QueryExpander != nil {
		if exp, err := h.cfg.QueryExpander.Expand(c.Request.Context(), tenantID, req.Query); err == nil && exp != "" {
			expandedQuery = exp
		}
	}
	fanOutReq := req
	fanOutReq.Query = expandedQuery
	streams, degraded, backendTimings := h.fanOut(c.Request.Context(), tenantID, fanOutReq, vec, topK)

	mergeStart := time.Now()
	merged := h.cfg.Merger.Merge(streams...)
	mergeMs := time.Since(mergeStart).Milliseconds()
	observability.ObserveBackendDuration("merge", time.Since(mergeStart).Seconds())

	// Snapshot the post-merge / pre-rerank score for each match so
	// the explain projection can surface the reranker's delta.
	preRerankByID := make(map[string]float32, len(merged))
	for _, m := range merged {
		preRerankByID[m.ID] = m.Score
	}

	rerankStart := time.Now()
	reranked, rerr := h.cfg.Reranker.Rerank(c.Request.Context(), req.Query, merged)
	rerankMs := time.Since(rerankStart).Milliseconds()
	observability.ObserveBackendDuration("rerank", time.Since(rerankStart).Seconds())
	if rerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rerank failed"})

		return
	}

	// Round-6 Task 1: optional MMR diversification after rerank,
	// before the policy filter so chunks rejected by ACL never
	// influence diversity.
	if req.Diversity > 0 && h.cfg.Diversifier != nil {
		reranked = h.cfg.Diversifier.Diversify(c.Request.Context(), reranked, req.Diversity, 0)
	}

	pres := h.cfg.PolicyFilter.Apply(reranked, privacyMode)
	// Phase 4: ACL + recipient gate. The snapshot's ACL drops chunks
	// the tenant has explicitly excluded; the recipient policy gates
	// the calling skill.
	postPolicy, blockedByACL, blockedByRecipient := applyPolicySnapshot(pres.Allowed, snapshot, req.SkillID)
	pres.Allowed = postPolicy
	pres.BlockedCount += blockedByACL + blockedByRecipient
	if len(pres.Allowed) > topK {
		pres.Allowed = pres.Allowed[:topK]
	}

	resp := RetrieveResponse{
		Hits: make([]RetrieveHit, 0, len(pres.Allowed)),
		Policy: RetrievePolicy{
			Applied:      appliedSources(streams),
			Degraded:     degraded,
			BlockedCount: pres.BlockedCount,
			PrivacyMode:  privacyMode,
		},
		Timings: timingsFromMap(backendTimings, mergeMs, rerankMs),
	}
	emitExplain := req.Explain && IsExplainAuthorized(c, h.cfg.ExplainEnvEnabled)
	for _, m := range pres.Allowed {
		hit := hitFromMatch(m)
		hit.PrivacyStrip = BuildPrivacyStrip(m, snapshot)
		if emitExplain {
			hit.Explain = BuildExplain(m, preRerankByID[m.ID])
		}
		resp.Hits = append(resp.Hits, hit)
	}
	resp.TraceID = observability.TraceID(c.Request.Context())

	// Cache the merged + reranked + filtered response. Errors are
	// swallowed — failing to write a cache entry must not fail the
	// retrieval.
	if h.cfg.Cache != nil && len(resp.Hits) > 0 {
		_ = h.cfg.Cache.Set(c.Request.Context(), tenantID, channelID, vec, scopeHash, cachedFromResponse(resp), h.cfg.CacheTTL)
	}

	h.applyDeviceFirst(c.Request.Context(), tenantID, channelID, privacyMode, req.DeviceTier, snapshot, &resp)
	c.JSON(http.StatusOK, resp)
}

// applyDeviceFirst computes the on-device-first hint and writes it
// onto resp. Caller invokes this after the response is otherwise
// fully populated. Lookup errors are swallowed (log and treat as
// no-shard) so a transient repository failure does not fail the
// retrieval.
//
// The channel-level allow-local toggle is sourced from the
// resolved PolicySnapshot. Per
// `docs/contracts/local-first-retrieval.md` the contract default
// is allow-local=true; PolicySnapshot expresses the inverse as
// `DenyLocalRetrieval` so an unconfigured channel
// (DenyLocalRetrieval==false) keeps the default-true semantics.
// When admins flip the channel toggle the request takes the
// `channel_disallowed` branch in policy.Decide.
func (h *Handler) applyDeviceFirst(ctx context.Context, tenantID, channelID, privacyMode, deviceTier string, snapshot policy.PolicySnapshot, resp *RetrieveResponse) {
	var shardVersion int64
	if h.cfg.ShardVersionLookup != nil {
		if v, lerr := h.cfg.ShardVersionLookup.LatestShardVersion(ctx, tenantID, channelID, privacyMode); lerr == nil {
			shardVersion = v
		}
	}
	decision := policy.Decide(policy.DeviceFirstInputs{
		DeviceTier:          policy.DeviceTier(deviceTier),
		AllowLocalRetrieval: !snapshot.DenyLocalRetrieval,
		PrivacyMode:         policy.PrivacyMode(privacyMode),
		LocalShardVersion:   shardVersion,
	})
	resp.PreferLocal = decision.PreferLocal
	resp.LocalShardVersion = decision.LocalShardVersion
	resp.PreferLocalReason = decision.Reason
}

// RetrieveWithSnapshot runs the retrieval pipeline against an
// explicit policy.PolicySnapshot, bypassing the configured
// PolicyResolver. It is the bridge the Phase 4 simulator's
// Retriever uses to compare live vs draft policy hits against the
// same corpus.
//
// The semantic cache is intentionally NOT consulted on this path:
// cached entries are gated by the live snapshot at write time, so
// reusing a cache entry for a draft snapshot would silently leak
// the live policy decisions into the simulated draft view. The
// pipeline still runs the privacy-label PolicyFilter, the ACL +
// recipient gate against the supplied snapshot, and rerank — i.e.
// every gate the gin handler applies, except cache I/O.
//
// req.PrivacyMode is honoured only as a fallback when both the
// supplied snapshot's EffectiveMode and HandlerConfig
// .DefaultPrivacyMode are empty (matching the gin handler's
// resolver-driven precedence).
//
// Live, cache-aware callers (the gin handler, the Phase 8 / Task 12
// batch retrieve fan-out) should use RetrieveWithSnapshotCached so
// they share the semantic cache with single-request /v1/retrieve.
func (h *Handler) RetrieveWithSnapshot(ctx context.Context, tenantID string, req RetrieveRequest, snapshot policy.PolicySnapshot) (RetrieveResponse, error) {
	topK, privacyMode, err := h.resolveTopKAndPrivacy(req, snapshot)
	if err != nil {
		return RetrieveResponse{}, err
	}
	if tenantID == "" {
		return RetrieveResponse{}, errors.New("retrieval: tenantID is required")
	}
	if req.Query == "" {
		return RetrieveResponse{}, errors.New("retrieval: query is required")
	}

	vec, err := h.cfg.Embedder.EmbedQuery(ctx, tenantID, req.Query)
	if err != nil {
		return RetrieveResponse{}, err
	}

	return h.runPipelineFromVec(ctx, tenantID, req, snapshot, vec, topK, privacyMode)
}

// RetrieveWithSnapshotCached is the cache-aware twin of
// RetrieveWithSnapshot. It is the entry point for live-snapshot
// callers that want the same semantic-cache short-circuit the gin
// handler applies (Phase 8 / Task 12 batch retrieve, in-process admin
// tooling). The pipeline is identical to RetrieveWithSnapshot — same
// fan-out, same policy gates — except for the cache I/O bookends:
//
//   - GET on entry. A hit re-runs the privacy-label PolicyFilter, ACL,
//     and recipient gates against the supplied snapshot before
//     returning, mirroring the gin handler so a policy change between
//     write and read can never serve content the new policy denies.
//   - SET on success. The merged + reranked + filtered response is
//     written back so the next caller (single-request OR batch) can
//     hit the cache.
//
// Pre-fix (Phase 8 / Task 12), batch retrieve called RetrieveWithSnapshot
// directly and bypassed the cache entirely — a documented performance
// regression on dashboards that fan a hot query out across N panels.
func (h *Handler) RetrieveWithSnapshotCached(ctx context.Context, tenantID string, req RetrieveRequest, snapshot policy.PolicySnapshot) (RetrieveResponse, error) {
	topK, privacyMode, err := h.resolveTopKAndPrivacy(req, snapshot)
	if err != nil {
		return RetrieveResponse{}, err
	}
	if tenantID == "" {
		return RetrieveResponse{}, errors.New("retrieval: tenantID is required")
	}
	if req.Query == "" {
		return RetrieveResponse{}, errors.New("retrieval: query is required")
	}

	vec, err := h.cfg.Embedder.EmbedQuery(ctx, tenantID, req.Query)
	if err != nil {
		return RetrieveResponse{}, err
	}

	channelID := firstNonEmpty(req.Channels)
	scopeHash := scopeHashFor(req, topK, privacyMode)

	if h.cfg.Cache != nil {
		cached, cerr := h.cfg.Cache.Get(ctx, tenantID, channelID, vec, scopeHash)
		if cerr == nil && cached != nil {
			filtered, blockedByPrivacy := filterCachedByPrivacyMode(cached, h.cfg.PolicyFilter, privacyMode)
			filtered, blockedByACL, blockedByRecipient := filterCachedBySnapshot(filtered, snapshot, req.SkillID)
			resp := responseFromCache(filtered, privacyMode, topK)
			resp.Policy.BlockedCount = blockedByPrivacy + blockedByACL + blockedByRecipient
			for i := range resp.Hits {
				resp.Hits[i].PrivacyStrip = BuildPrivacyStrip(matchFromCachedHit(resp.Hits[i]), snapshot)
			}
			h.applyDeviceFirst(ctx, tenantID, channelID, privacyMode, req.DeviceTier, snapshot, &resp)
			return resp, nil
		}
	}

	resp, perr := h.runPipelineFromVec(ctx, tenantID, req, snapshot, vec, topK, privacyMode)
	if perr != nil {
		return RetrieveResponse{}, perr
	}
	if h.cfg.Cache != nil && len(resp.Hits) > 0 {
		_ = h.cfg.Cache.Set(ctx, tenantID, channelID, vec, scopeHash, cachedFromResponse(resp), h.cfg.CacheTTL)
	}
	h.applyDeviceFirst(ctx, tenantID, channelID, privacyMode, req.DeviceTier, snapshot, &resp)
	return resp, nil
}

// resolveTopKAndPrivacy applies the same topK / privacyMode resolution
// rules the gin handler uses, lifted out so the snapshot-driven
// callers (RetrieveWithSnapshot and RetrieveWithSnapshotCached) can
// share them verbatim.
func (h *Handler) resolveTopKAndPrivacy(req RetrieveRequest, snapshot policy.PolicySnapshot) (int, string, error) {
	topK := req.TopK
	if topK == 0 {
		topK = h.cfg.DefaultTopK
	}
	if topK < 0 {
		return 0, "", errors.New("retrieval: top_k must be >= 0")
	}
	if topK > h.cfg.MaxTopK {
		topK = h.cfg.MaxTopK
	}
	privacyMode := req.PrivacyMode
	if privacyMode == "" {
		privacyMode = h.cfg.DefaultPrivacyMode
	}
	if snapshot.EffectiveMode != "" {
		privacyMode = string(snapshot.EffectiveMode)
	}
	return topK, privacyMode, nil
}

// runPipelineFromVec is the cache-free inner pipeline shared by
// RetrieveWithSnapshot and RetrieveWithSnapshotCached's miss path.
// It expects the query to already be embedded and the topK /
// privacyMode resolution to be done by the caller.
func (h *Handler) runPipelineFromVec(ctx context.Context, tenantID string, req RetrieveRequest, snapshot policy.PolicySnapshot, vec []float32, topK int, privacyMode string) (RetrieveResponse, error) {
	streams, degraded, backendTimings := h.fanOut(ctx, tenantID, req, vec, topK)

	mergeStart := time.Now()
	merged := h.cfg.Merger.Merge(streams...)
	mergeMs := time.Since(mergeStart).Milliseconds()
	rerankStart := time.Now()
	reranked, rerr := h.cfg.Reranker.Rerank(ctx, req.Query, merged)
	rerankMs := time.Since(rerankStart).Milliseconds()
	if rerr != nil {
		return RetrieveResponse{}, rerr
	}

	if req.Diversity > 0 && h.cfg.Diversifier != nil {
		reranked = h.cfg.Diversifier.Diversify(ctx, reranked, req.Diversity, 0)
	}

	pres := h.cfg.PolicyFilter.Apply(reranked, privacyMode)
	postPolicy, blockedByACL, blockedByRecipient := applyPolicySnapshot(pres.Allowed, snapshot, req.SkillID)
	pres.Allowed = postPolicy
	pres.BlockedCount += blockedByACL + blockedByRecipient
	if len(pres.Allowed) > topK {
		pres.Allowed = pres.Allowed[:topK]
	}

	resp := RetrieveResponse{
		Hits: make([]RetrieveHit, 0, len(pres.Allowed)),
		Policy: RetrievePolicy{
			Applied:      appliedSources(streams),
			Degraded:     degraded,
			BlockedCount: pres.BlockedCount,
			PrivacyMode:  privacyMode,
		},
		Timings: timingsFromMap(backendTimings, mergeMs, rerankMs),
	}
	for _, m := range pres.Allowed {
		hit := hitFromMatch(m)
		hit.PrivacyStrip = BuildPrivacyStrip(m, snapshot)
		resp.Hits = append(resp.Hits, hit)
	}
	return resp, nil
}

// fanOut runs every wired-in backend in parallel under
// errgroup.WithContext, applying a per-backend deadline. Returns the
// per-stream []*Match list, the list of degraded sources, and the
// per-backend duration map keyed by backend name (vector/bm25/
// graph/memory).
func (h *Handler) fanOut(ctx context.Context, tenantID string, req RetrieveRequest, vec []float32, topK int) ([][]*Match, []string, map[string]time.Duration) {
	ctx, fanSpan := observability.StartSpan(ctx, "retrieval.fanout",
		observability.AttrTenantID.String(tenantID),
	)
	defer fanSpan.End()

	g, gctx := errgroup.WithContext(ctx)

	var (
		mu       sync.Mutex
		streams  = make([][]*Match, 0, 4)
		degraded []string
		timings  = make(map[string]time.Duration, 4)
	)

	push := func(ms []*Match) {
		mu.Lock()
		defer mu.Unlock()
		streams = append(streams, ms)
	}
	markDegraded := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		degraded = append(degraded, name)
	}
	recordTiming := func(name string, d time.Duration, hits int) {
		mu.Lock()
		timings[name] = d
		mu.Unlock()
		observability.ObserveBackendDuration(name, d.Seconds())
		observability.SetBackendHits(name, hits)
	}

	deadlineCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(gctx, h.cfg.PerBackendDeadline)
	}

	if h.cfg.VectorStore != nil {
		g.Go(func() error {
			ctx, cancel := deadlineCtx()
			defer cancel()
			ctx, span := observability.StartSpan(ctx, "retrieval.vector",
				observability.AttrBackend.String(SourceVector),
			)
			defer span.End()
			start := time.Now()
			hits, err := h.cfg.VectorStore.Search(ctx, tenantID, vec, storage.SearchOpts{
				Limit:  topK,
				Filter: buildFilter(req),
			})
			elapsed := time.Since(start)
			span.SetAttributes(observability.AttrLatencyMs.Int64(elapsed.Milliseconds()))
			if err != nil {
				observability.RecordError(span, err)
				markDegraded(SourceVector)
				recordTiming(SourceVector, elapsed, 0)

				return nil
			}
			out := make([]*Match, 0, len(hits))
			for i, hit := range hits {
				m := matchFromQdrant(hit)
				m.Rank = i + 1
				out = append(out, m)
			}
			span.SetAttributes(observability.AttrHitCount.Int(len(out)))
			recordTiming(SourceVector, elapsed, len(out))
			push(out)

			return nil
		})
	}
	if h.cfg.BM25 != nil {
		g.Go(func() error {
			ctx, cancel := deadlineCtx()
			defer cancel()
			ctx, span := observability.StartSpan(ctx, "retrieval.bm25",
				observability.AttrBackend.String(SourceBM25),
			)
			defer span.End()
			start := time.Now()
			hits, err := h.cfg.BM25.Search(ctx, tenantID, req.Query, topK)
			elapsed := time.Since(start)
			span.SetAttributes(observability.AttrLatencyMs.Int64(elapsed.Milliseconds()))
			if err != nil {
				observability.RecordError(span, err)
				markDegraded(SourceBM25)
				recordTiming(SourceBM25, elapsed, 0)

				return nil
			}
			span.SetAttributes(observability.AttrHitCount.Int(len(hits)))
			for i, m := range hits {
				if m.Source == "" {
					m.Source = SourceBM25
				}
				if m.Rank == 0 {
					m.Rank = i + 1
				}
			}
			recordTiming(SourceBM25, elapsed, len(hits))
			push(hits)

			return nil
		})
	}
	if h.cfg.Graph != nil {
		g.Go(func() error {
			ctx, cancel := deadlineCtx()
			defer cancel()
			ctx, span := observability.StartSpan(ctx, "retrieval.graph",
				observability.AttrBackend.String(SourceGraph),
			)
			defer span.End()
			start := time.Now()
			hits, err := h.cfg.Graph.Traverse(ctx, tenantID, req.Query, topK)
			elapsed := time.Since(start)
			span.SetAttributes(observability.AttrLatencyMs.Int64(elapsed.Milliseconds()))
			if err != nil {
				observability.RecordError(span, err)
				markDegraded(SourceGraph)
				recordTiming(SourceGraph, elapsed, 0)

				return nil
			}
			span.SetAttributes(observability.AttrHitCount.Int(len(hits)))
			for i, m := range hits {
				if m.Source == "" {
					m.Source = SourceGraph
				}
				if m.Rank == 0 {
					m.Rank = i + 1
				}
			}
			recordTiming(SourceGraph, elapsed, len(hits))
			push(hits)

			return nil
		})
	}
	if h.cfg.Memory != nil {
		g.Go(func() error {
			ctx, cancel := deadlineCtx()
			defer cancel()
			ctx, span := observability.StartSpan(ctx, "retrieval.memory",
				observability.AttrBackend.String(SourceMemory),
			)
			defer span.End()
			start := time.Now()
			hits, err := h.cfg.Memory.Search(ctx, tenantID, req.Query, topK)
			elapsed := time.Since(start)
			span.SetAttributes(observability.AttrLatencyMs.Int64(elapsed.Milliseconds()))
			if err != nil {
				observability.RecordError(span, err)
				markDegraded(SourceMemory)
				recordTiming(SourceMemory, elapsed, 0)

				return nil
			}
			span.SetAttributes(observability.AttrHitCount.Int(len(hits)))
			for i, m := range hits {
				if m.Source == "" {
					m.Source = SourceMemory
				}
				if m.Rank == 0 {
					m.Rank = i + 1
				}
			}
			recordTiming(SourceMemory, elapsed, len(hits))
			push(hits)

			return nil
		})
	}

	_ = g.Wait()

	return streams, degraded, timings
}

// matchFromQdrant projects a *storage.QdrantHit into the merger's
// neutral *Match shape.
func matchFromQdrant(h storage.QdrantHit) *Match {
	m := &Match{
		ID:     h.ID,
		Source: SourceVector,
		Score:  h.Score,
	}
	for k, v := range h.Payload {
		switch k {
		case "tenant_id":
			m.TenantID, _ = v.(string)
		case "source_id":
			m.SourceID, _ = v.(string)
		case "document_id":
			m.DocumentID, _ = v.(string)
		case "block_id":
			m.BlockID, _ = v.(string)
		case "title":
			m.Title, _ = v.(string)
		case "uri":
			m.URI, _ = v.(string)
		case "privacy_label":
			m.PrivacyLabel, _ = v.(string)
		case "text":
			m.Text, _ = v.(string)
		case "connector":
			m.Connector, _ = v.(string)
		case "ingested_at":
			if s, ok := v.(string); ok {
				if t, perr := time.Parse(time.RFC3339, s); perr == nil {
					m.IngestedAt = t
				}
			}
		default:
			if m.Metadata == nil {
				m.Metadata = map[string]any{}
			}
			m.Metadata[k] = v
		}
	}

	return m
}

// hitFromMatch projects an internal *Match back into the public
// response shape.
func hitFromMatch(m *Match) RetrieveHit {
	return RetrieveHit{
		ID:           m.ID,
		Score:        m.Score,
		TenantID:     m.TenantID,
		SourceID:     m.SourceID,
		DocumentID:   m.DocumentID,
		BlockID:      m.BlockID,
		Title:        m.Title,
		URI:          m.URI,
		PrivacyLabel: m.PrivacyLabel,
		Text:         m.Text,
		Connector:    m.Connector,
		Metadata:     m.Metadata,
		Sources:      m.Sources,
	}
}

// responseFromCache projects a cached entry into the public response
// shape, stamping policy.cache_hit so clients can render the badge.
// The result is sliced to topK as a defense-in-depth — scopeHashFor
// already keys the cache on topK, but a stale entry written by an
// older binary (or a future schema bump) must never violate the API
// contract.
func responseFromCache(c *storage.CachedResult, privacyMode string, topK int) RetrieveResponse {
	hits := c.Hits
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	out := RetrieveResponse{
		Hits: make([]RetrieveHit, 0, len(hits)),
		Policy: RetrievePolicy{
			CacheHit:    true,
			PrivacyMode: privacyMode,
		},
	}
	for _, h := range hits {
		out.Hits = append(out.Hits, RetrieveHit{
			ID:           h.ID,
			Score:        h.Score,
			TenantID:     h.TenantID,
			SourceID:     h.SourceID,
			DocumentID:   h.DocumentID,
			BlockID:      h.BlockID,
			Title:        h.Title,
			URI:          h.URI,
			PrivacyLabel: h.PrivacyLabel,
			Text:         h.Text,
			Connector:    h.Connector,
			Metadata:     h.Metadata,
		})
	}

	return out
}

// cachedFromResponse projects the response shape into the storage-
// layer CachedResult so the cache is populated without re-fetching
// chunk metadata.
func cachedFromResponse(resp RetrieveResponse) *storage.CachedResult {
	c := &storage.CachedResult{
		Hits:     make([]storage.CachedHit, 0, len(resp.Hits)),
		ChunkIDs: make([]string, 0, len(resp.Hits)),
	}
	for _, h := range resp.Hits {
		c.Hits = append(c.Hits, storage.CachedHit{
			ID:           h.ID,
			Score:        h.Score,
			TenantID:     h.TenantID,
			SourceID:     h.SourceID,
			DocumentID:   h.DocumentID,
			BlockID:      h.BlockID,
			Title:        h.Title,
			URI:          h.URI,
			PrivacyLabel: h.PrivacyLabel,
			Text:         h.Text,
			Connector:    h.Connector,
			Metadata:     h.Metadata,
		})
		c.ChunkIDs = append(c.ChunkIDs, h.ID)
	}

	return c
}

// timingsFromMap projects the per-backend duration map produced by
// fanOut into the public RetrieveTimings shape exposed on
// RetrieveResponse. Missing entries default to 0 (backend not
// configured).
func timingsFromMap(t map[string]time.Duration, mergeMs, rerankMs int64) RetrieveTimings {
	ms := func(name string) int64 {
		if d, ok := t[name]; ok {
			return d.Milliseconds()
		}
		return 0
	}
	return RetrieveTimings{
		VectorMs: ms(SourceVector),
		BM25Ms:   ms(SourceBM25),
		GraphMs:  ms(SourceGraph),
		MemoryMs: ms(SourceMemory),
		MergeMs:  mergeMs,
		RerankMs: rerankMs,
	}
}

// appliedSources returns the unique list of backends that contributed
// at least one match across the streams.
func appliedSources(streams [][]*Match) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, s := range streams {
		for _, m := range s {
			if m == nil || m.Source == "" {
				continue
			}
			if _, ok := seen[m.Source]; ok {
				continue
			}
			seen[m.Source] = struct{}{}
			out = append(out, m.Source)
		}
	}

	return out
}

// scopeHashFor produces a stable hash over the request fields that
// affect the retrieval scope. Same hash → same cache key.
//
// topK is part of the scope: a topK=5 request must NOT serve from a
// topK=100 entry (would return more rows than asked) and a topK=100
// request must NOT serve from a topK=5 entry (would return fewer
// rows than asked).
//
// SkillID is part of the scope so a skill denied by recipient policy
// can never be served a cached response originally produced for an
// allowed skill — without the SkillID component, two requests with
// different SkillIDs but identical query/channel/topK would share a
// cache slot and bypass the recipient gate.
//
// privacyMode is the *resolved* effective mode (post-PolicyResolver),
// NOT req.PrivacyMode. Hashing the raw request field would let two
// requests with the same req.PrivacyMode but different resolver-
// determined effective modes (e.g. after an admin tightens the
// tenant policy from "remote" to "local-only") share a cache slot —
// stale entries from the more permissive mode would keep serving
// until the TTL expired. The resolved mode closes that window.
//
// Bump this hash whenever request fields that affect the result set
// are added.
func scopeHashFor(req RetrieveRequest, topK int, privacyMode string) string {
	h := sha256.New()
	for _, v := range req.Sources {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write([]byte("|channels|"))
	for _, v := range req.Channels {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write([]byte("|documents|"))
	for _, v := range req.Documents {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write([]byte("|privacy|"))
	_, _ = h.Write([]byte(privacyMode))
	_, _ = h.Write([]byte("|skill|"))
	_, _ = h.Write([]byte(req.SkillID))
	_, _ = h.Write([]byte("|topk|"))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(topK))
	_, _ = h.Write(buf[:])
	// Diversity (Round-6 Task 1) re-ranks the result set via MMR,
	// so two requests with the same query but different diversity
	// lambdas MUST NOT share a cache slot — otherwise a request
	// asking for diverse results would be served the cached pure-
	// relevance ordering (or vice versa).
	_, _ = h.Write([]byte("|diversity|"))
	binary.BigEndian.PutUint32(buf[:4], math.Float32bits(req.Diversity))
	_, _ = h.Write(buf[:4])
	// Experiment routing (Round-6 Task 10) swaps the active
	// retrieval config (different reranker, fan-out weights,
	// etc.), which changes the result set. Including the
	// experiment name and bucket key keeps control/variant
	// responses from cross-contaminating each other's cache slot.
	_, _ = h.Write([]byte("|experiment|"))
	_, _ = h.Write([]byte(req.ExperimentName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.ExperimentBucketKey))

	return hex.EncodeToString(h.Sum(nil))
}

// firstNonEmpty returns the first non-empty entry of slice or "" if
// none present.
func firstNonEmpty(slice []string) string {
	for _, s := range slice {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}

	return ""
}

// tenantIDFromContext mirrors the audit-handler implementation so the
// authentication contract is identical across handlers.
func tenantIDFromContext(c *gin.Context) (string, bool) {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}

	return s, true
}

// buildFilter projects the optional request fields into a Qdrant
// filter object. Returns nil if no filters were set.
func buildFilter(req RetrieveRequest) map[string]any {
	conds := []map[string]any{}
	add := func(key string, vals []string) {
		if len(vals) == 0 {
			return
		}
		any_ := []map[string]any{}
		for _, v := range vals {
			any_ = append(any_, map[string]any{"key": key, "match": map[string]any{"value": v}})
		}
		conds = append(conds, map[string]any{"should": any_})
	}
	add("source_id", req.Sources)
	add("namespace_id", req.Channels)
	add("document_id", req.Documents)
	if len(conds) == 0 {
		return nil
	}

	return map[string]any{"must": conds}
}
