// abtest_results.go — Round-7 Task 6.
//
// Aggregates the recorded query_analytics rows grouped by
// experiment arm. Reports per-arm count, avg latency, avg hit
// count, cache hit ratio, P50, and P95 so operators can decide
// whether to promote the variant or roll the experiment back.
//
// The handler mounts GET /v1/admin/retrieval/experiments/:name/results
// and is tenant-scoped via the standard tenantIDFromContext path.
package admin

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// ABTestArmResult is the per-arm aggregate.
type ABTestArmResult struct {
	Arm             string  `json:"arm"`
	Count           int     `json:"count"`
	AvgLatencyMS    float64 `json:"avg_latency_ms"`
	AvgHitCount     float64 `json:"avg_hit_count"`
	CacheHitPercent float64 `json:"cache_hit_percent"`
	P50LatencyMS    float64 `json:"p50_latency_ms"`
	P95LatencyMS    float64 `json:"p95_latency_ms"`
}

// ABTestResultsResponse is the JSON envelope for the endpoint.
type ABTestResultsResponse struct {
	Experiment string            `json:"experiment"`
	Arms       []ABTestArmResult `json:"arms"`
}

// ABTestResultsAggregator groups analytics rows by experiment arm.
type ABTestResultsAggregator struct {
	store QueryAnalyticsStore
}

// NewABTestResultsAggregator validates inputs.
func NewABTestResultsAggregator(store QueryAnalyticsStore) (*ABTestResultsAggregator, error) {
	if store == nil {
		return nil, errors.New("abtest_results: nil store")
	}
	return &ABTestResultsAggregator{store: store}, nil
}

// Aggregate returns per-arm aggregates for (tenantID, experiment).
// since/until bound the analytics window; both may be zero.
func (a *ABTestResultsAggregator) Aggregate(ctx context.Context, tenantID, experiment string, since, until time.Time) (*ABTestResultsResponse, error) {
	if tenantID == "" {
		return nil, errors.New("abtest_results: missing tenant_id")
	}
	if experiment == "" {
		return nil, errors.New("abtest_results: missing experiment")
	}
	rows, err := a.store.List(ctx, QueryAnalyticsQuery{TenantID: tenantID, Since: since, Until: until, Limit: 10000})
	if err != nil {
		return nil, err
	}
	type bucket struct {
		latencies  []float64
		hits       int64
		cacheHits  int
		count      int
		latencySum float64
	}
	byArm := map[string]*bucket{}
	for _, r := range rows {
		if r.ExperimentName != experiment {
			continue
		}
		arm := r.ExperimentArm
		if arm == "" {
			arm = "control"
		}
		b, ok := byArm[arm]
		if !ok {
			b = &bucket{}
			byArm[arm] = b
		}
		b.count++
		b.latencies = append(b.latencies, float64(r.LatencyMS))
		b.latencySum += float64(r.LatencyMS)
		b.hits += int64(r.HitCount)
		if r.CacheHit {
			b.cacheHits++
		}
	}
	resp := &ABTestResultsResponse{Experiment: experiment, Arms: make([]ABTestArmResult, 0, len(byArm))}
	for arm, b := range byArm {
		if b.count == 0 {
			continue
		}
		p50 := percentile(b.latencies, 0.50)
		p95 := percentile(b.latencies, 0.95)
		resp.Arms = append(resp.Arms, ABTestArmResult{
			Arm:             arm,
			Count:           b.count,
			AvgLatencyMS:    b.latencySum / float64(b.count),
			AvgHitCount:     float64(b.hits) / float64(b.count),
			CacheHitPercent: float64(b.cacheHits) / float64(b.count) * 100.0,
			P50LatencyMS:    p50,
			P95LatencyMS:    p95,
		})
	}
	// Deterministic ordering: control, variant, then alphabetical.
	sort.Slice(resp.Arms, func(i, j int) bool {
		if resp.Arms[i].Arm == "control" && resp.Arms[j].Arm != "control" {
			return true
		}
		if resp.Arms[j].Arm == "control" && resp.Arms[i].Arm != "control" {
			return false
		}
		return resp.Arms[i].Arm < resp.Arms[j].Arm
	})
	return resp, nil
}

// percentile returns the p-th percentile of vals (0.0..1.0).
// Returns 0 for an empty slice; uses nearest-rank otherwise.
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64{}, vals...)
	sort.Float64s(cp)
	rank := int(math.Ceil(p * float64(len(cp))))
	if rank <= 0 {
		rank = 1
	}
	if rank > len(cp) {
		rank = len(cp)
	}
	return cp[rank-1]
}

// ABTestResultsHandler exposes the aggregator over HTTP.
type ABTestResultsHandler struct {
	agg *ABTestResultsAggregator
}

// NewABTestResultsHandler validates inputs.
func NewABTestResultsHandler(agg *ABTestResultsAggregator) (*ABTestResultsHandler, error) {
	if agg == nil {
		return nil, errors.New("abtest_results: nil aggregator")
	}
	return &ABTestResultsHandler{agg: agg}, nil
}

// Register mounts GET /v1/admin/retrieval/experiments/:name/results.
func (h *ABTestResultsHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/retrieval/experiments/:name/results", h.get)
}

func (h *ABTestResultsHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	name := c.Param("name")
	var since, until time.Time
	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
		since = t
	}
	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
		until = t
	}
	resp, err := h.agg.Aggregate(c.Request.Context(), tenantID, name, since, until)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}
