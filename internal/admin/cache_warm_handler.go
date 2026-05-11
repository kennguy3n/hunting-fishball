// cache_warm_handler.go — Round-7 Task 9.
//
// POST /v1/admin/retrieval/warm-cache accepts a list of
// (tenant, query) tuples and runs them through the retrieval
// handler. The handler's existing post-retrieve cache.Set path
// populates Redis as a side effect of the call.
//
// When `auto` is true and no tuples are supplied, the handler
// derives the top-N queries from the query_analytics recorder
// (Round-7 Task 4) and warms those automatically.
package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// CacheWarmTuple mirrors retrieval.WarmTuple for the JSON wire.
type CacheWarmTuple struct {
	TenantID    string   `json:"-"`
	Query       string   `json:"query"`
	TopK        int      `json:"top_k,omitempty"`
	Channels    []string `json:"channels,omitempty"`
	PrivacyMode string   `json:"privacy_mode,omitempty"`
}

// CacheWarmResult is the per-tuple outcome.
type CacheWarmResult struct {
	Query   string        `json:"query"`
	Hits    int           `json:"hits"`
	Latency time.Duration `json:"latency"`
	Error   string        `json:"error,omitempty"`
}

// CacheWarmSummary aggregates the CacheWarmResult slice.
type CacheWarmSummary struct {
	Total     int               `json:"total"`
	Succeeded int               `json:"succeeded"`
	Failed    int               `json:"failed"`
	Results   []CacheWarmResult `json:"results"`
}

// CacheWarmerExecutor is the narrow surface the admin handler
// uses. The real implementation lives in internal/retrieval and
// is wired in cmd/api so this package stays free of a retrieval
// import (which would create a cycle through abtest_router).
type CacheWarmerExecutor interface {
	Warm(ctx context.Context, tuples []CacheWarmTuple) CacheWarmSummary
}

// CacheWarmHandler is the admin HTTP surface.
type CacheWarmHandler struct {
	warmer     CacheWarmerExecutor
	analytics  QueryAnalyticsStore
	audit      AuditWriter
	autoTopN   int
	autoWindow time.Duration
}

// CacheWarmHandlerConfig wires the dependencies.
type CacheWarmHandlerConfig struct {
	Warmer     CacheWarmerExecutor
	Analytics  QueryAnalyticsStore
	Audit      AuditWriter
	AutoTopN   int
	AutoWindow time.Duration
}

// NewCacheWarmHandler validates inputs.
func NewCacheWarmHandler(cfg CacheWarmHandlerConfig) (*CacheWarmHandler, error) {
	if cfg.Warmer == nil {
		return nil, errors.New("cache_warm: nil warmer")
	}
	if cfg.AutoTopN <= 0 {
		cfg.AutoTopN = 10
	}
	if cfg.AutoWindow <= 0 {
		cfg.AutoWindow = 24 * time.Hour
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	return &CacheWarmHandler{
		warmer:     cfg.Warmer,
		analytics:  cfg.Analytics,
		audit:      cfg.Audit,
		autoTopN:   cfg.AutoTopN,
		autoWindow: cfg.AutoWindow,
	}, nil
}

// Register mounts POST /v1/admin/retrieval/warm-cache.
func (h *CacheWarmHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/retrieval/warm-cache", h.warm)
}

// CacheWarmRequest is the JSON body shape.
type CacheWarmRequest struct {
	Tuples []CacheWarmTuple `json:"tuples,omitempty"`
	Auto   bool             `json:"auto,omitempty"`
	TopN   int              `json:"top_n,omitempty"`
}

func (h *CacheWarmHandler) warm(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req CacheWarmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Allow empty body when auto=true via query param.
		if c.Query("auto") != "true" {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Auto = true
	}
	tuples := make([]CacheWarmTuple, 0, len(req.Tuples))
	for _, t := range req.Tuples {
		tuples = append(tuples, CacheWarmTuple{
			TenantID:    tenantID,
			Query:       t.Query,
			TopK:        t.TopK,
			Channels:    t.Channels,
			PrivacyMode: t.PrivacyMode,
		})
	}
	if (req.Auto || len(tuples) == 0) && h.analytics != nil {
		limit := req.TopN
		if limit <= 0 {
			limit = h.autoTopN
		}
		since := time.Now().UTC().Add(-h.autoWindow)
		top, err := h.analytics.TopQueries(c.Request.Context(), QueryAnalyticsQuery{TenantID: tenantID, Since: since, Limit: limit})
		if err == nil {
			for _, q := range top {
				if q.SampleText == "" {
					continue
				}
				tuples = append(tuples, CacheWarmTuple{TenantID: tenantID, Query: q.SampleText})
			}
		}
	}
	if len(tuples) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no tuples to warm"})
		return
	}
	summary := h.warmer.Warm(c.Request.Context(), tuples)
	actorID := actorIDFromContext(c)
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actorID, audit.ActionRetrievalCacheWarmed, "retrieval", "",
		audit.JSONMap{"total": summary.Total, "succeeded": summary.Succeeded, "failed": summary.Failed}, "",
	))
	c.JSON(http.StatusOK, summary)
}

// CacheWarmExecutorFunc adapts a function to the
// CacheWarmerExecutor interface. Useful for tests.
type CacheWarmExecutorFunc func(ctx context.Context, tuples []CacheWarmTuple) CacheWarmSummary

// Warm implements CacheWarmerExecutor.
func (f CacheWarmExecutorFunc) Warm(ctx context.Context, tuples []CacheWarmTuple) CacheWarmSummary {
	return f(ctx, tuples)
}
