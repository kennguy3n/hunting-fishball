// pipeline_throughput_handler.go — Round-14 Task 4.
//
// GET /v1/admin/pipeline/throughput?window=5m returns per-stage
// (fetch/parse/embed/store) event counts and average latency for
// the requested rolling window.
//
// The handler reads from a process-local PipelineThroughputRecorder
// that keeps a 60-bucket / 1-minute-per-bucket ring. Production
// wiring records every stage completion via Record() — buckets
// older than 60 minutes drop off, so the recorder is bounded.
//
// We deliberately do NOT serve this from Prometheus: the per-
// stage duration histograms already exist in metrics.go, but
// computing percentiles and averages out of histogram_quantile
// requires the full Prom-server stack. Operators want a quick
// in-process sparkline; this endpoint provides that without a
// dependency on a deployed Prometheus.
package admin

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// throughputBucketCount caps the recorder ring to 60 buckets.
	throughputBucketCount = 60
	// throughputBucketSize is one minute per bucket.
	throughputBucketSize = time.Minute
)

// PipelineThroughputRecorder maintains a 60-bucket ring per stage.
type PipelineThroughputRecorder struct {
	mu      sync.Mutex
	buckets map[string][]throughputBucket
	nowFn   func() time.Time
}

type throughputBucket struct {
	at        time.Time
	count     int
	latencyUS int64
}

// NewPipelineThroughputRecorder constructs an empty recorder.
func NewPipelineThroughputRecorder() *PipelineThroughputRecorder {
	return &PipelineThroughputRecorder{
		buckets: map[string][]throughputBucket{},
		nowFn:   func() time.Time { return time.Now().UTC() },
	}
}

// Record adds an observation. stage is one of fetch/parse/embed/
// store (the bounded set the pipeline emits) but the recorder
// also accepts custom names so chaos / load tests can probe with
// arbitrary stage labels.
func (r *PipelineThroughputRecorder) Record(stage string, latency time.Duration) {
	if r == nil || stage == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFn().Truncate(throughputBucketSize)
	buckets := r.buckets[stage]
	// Append to the current bucket if present, otherwise start
	// a new one.
	if len(buckets) == 0 || !buckets[len(buckets)-1].at.Equal(now) {
		buckets = append(buckets, throughputBucket{at: now})
	}
	last := len(buckets) - 1
	buckets[last].count++
	if latency > 0 {
		buckets[last].latencyUS += latency.Microseconds()
	}
	// Drop buckets that have fallen out of the retention window.
	cutoff := now.Add(-time.Duration(throughputBucketCount-1) * throughputBucketSize)
	keep := buckets[:0]
	for _, b := range buckets {
		if !b.at.Before(cutoff) {
			keep = append(keep, b)
		}
	}
	r.buckets[stage] = keep
}

// PipelineThroughputRow is the per-stage projection.
type PipelineThroughputRow struct {
	Stage       string  `json:"stage"`
	Count       int     `json:"count"`
	AvgLatency  float64 `json:"avg_latency_ms"`
	BucketCount int     `json:"bucket_count"`
}

// Window returns one row per known stage covering the last
// `window` duration. A window <= 0 returns every retained bucket.
func (r *PipelineThroughputRecorder) Window(window time.Duration) []PipelineThroughputRow {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Time{}
	if window > 0 {
		cutoff = r.nowFn().Add(-window)
	}
	out := make([]PipelineThroughputRow, 0, len(r.buckets))
	for stage, buckets := range r.buckets {
		var count int
		var latency int64
		var bc int
		for _, b := range buckets {
			if !cutoff.IsZero() && b.at.Before(cutoff) {
				continue
			}
			count += b.count
			latency += b.latencyUS
			bc++
		}
		avg := 0.0
		if count > 0 {
			avg = float64(latency) / float64(count) / 1000.0
		}
		out = append(out, PipelineThroughputRow{
			Stage: stage, Count: count, AvgLatency: avg, BucketCount: bc,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })
	return out
}

// PipelineThroughputHandler serves the admin endpoint.
type PipelineThroughputHandler struct {
	rec *PipelineThroughputRecorder
}

// NewPipelineThroughputHandler binds the handler to rec. nil is
// tolerated and yields an empty response.
func NewPipelineThroughputHandler(rec *PipelineThroughputRecorder) *PipelineThroughputHandler {
	return &PipelineThroughputHandler{rec: rec}
}

// Register mounts the endpoint on rg.
func (h *PipelineThroughputHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/pipeline/throughput", h.get)
}

// PipelineThroughputResponse is the JSON envelope.
type PipelineThroughputResponse struct {
	Window string                  `json:"window"`
	Stages []PipelineThroughputRow `json:"stages"`
}

func (h *PipelineThroughputHandler) get(c *gin.Context) {
	window := 5 * time.Minute
	if raw := c.Query("window"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 && d <= time.Hour {
			window = d
		}
	}
	rows := h.rec.Window(window)
	c.JSON(http.StatusOK, PipelineThroughputResponse{Window: window.String(), Stages: rows})
}
