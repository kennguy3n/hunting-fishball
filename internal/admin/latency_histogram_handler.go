// latency_histogram_handler.go — Round-14 Task 2.
//
// GET /v1/admin/retrieval/latency-histogram returns P50/P75/P90/
// P95/P99 latency for the last N minutes, broken down by
// backend (vector / bm25 / graph / memory / merge / rerank /
// total). The handler reads from a process-local
// LatencyHistogramRecorder so the percentile math runs without
// having to talk to Prometheus.
//
// The recorder keeps a bounded ring of recent samples per
// backend; older samples drop off as the window slides. This is
// deliberately a sampler rather than an exact histogram: the
// goal is operator-facing percentile estimates, not SLO-grade
// math. Cluster-wide SLO percentiles still come from Prometheus.
package admin

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// defaultLatencyHistogramCapacity bounds the per-backend ring so a
// hot tenant cannot exhaust API memory. 4096 samples per backend
// at 7 backends ≈ 30 KB of float64s — cheap.
const defaultLatencyHistogramCapacity = 4096

// LatencyHistogramRecorder holds a ring of recent latency
// observations per backend. The retrieval handler feeds it via
// Observe() on each request completion.
type LatencyHistogramRecorder struct {
	mu      sync.Mutex
	cap     int
	samples map[string][]latencySample
	nowFn   func() time.Time
}

type latencySample struct {
	at time.Time
	ms float64
}

// NewLatencyHistogramRecorder constructs a recorder. capacity<=0
// falls back to defaultLatencyHistogramCapacity.
func NewLatencyHistogramRecorder(capacity int) *LatencyHistogramRecorder {
	if capacity <= 0 {
		capacity = defaultLatencyHistogramCapacity
	}
	return &LatencyHistogramRecorder{
		cap:     capacity,
		samples: map[string][]latencySample{},
		nowFn:   func() time.Time { return time.Now().UTC() },
	}
}

// Observe records one latency sample on the named backend ring.
// Backends not previously seen are auto-registered.
func (r *LatencyHistogramRecorder) Observe(backend string, latency time.Duration) {
	if r == nil || backend == "" || latency < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ring := r.samples[backend]
	ring = append(ring, latencySample{at: r.nowFn(), ms: float64(latency.Microseconds()) / 1000.0})
	if len(ring) > r.cap {
		// Drop the oldest entries to keep memory bounded.
		ring = ring[len(ring)-r.cap:]
	}
	r.samples[backend] = ring
}

// LatencyPercentiles is the per-backend projection.
type LatencyPercentiles struct {
	Backend string  `json:"backend"`
	Count   int     `json:"count"`
	P50     float64 `json:"p50_ms"`
	P75     float64 `json:"p75_ms"`
	P90     float64 `json:"p90_ms"`
	P95     float64 `json:"p95_ms"`
	P99     float64 `json:"p99_ms"`
}

// percentiles returns P50/P75/P90/P95/P99 over the sorted ms
// slice. The slice must already be sorted ascending.
func percentiles(sorted []float64) (p50, p75, p90, p95, p99 float64) {
	if len(sorted) == 0 {
		return 0, 0, 0, 0, 0
	}
	q := func(p float64) float64 {
		if len(sorted) == 1 {
			return sorted[0]
		}
		// Nearest-rank: idx = ceil(p * N) - 1, clamped.
		idx := int(p*float64(len(sorted)) + 0.5)
		if idx < 1 {
			idx = 1
		}
		if idx > len(sorted) {
			idx = len(sorted)
		}
		return sorted[idx-1]
	}
	return q(0.50), q(0.75), q(0.90), q(0.95), q(0.99)
}

// SinceMinutes returns the percentiles per backend for the last
// `since` window. since<=0 returns every retained sample.
func (r *LatencyHistogramRecorder) SinceMinutes(since time.Duration) []LatencyPercentiles {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Time{}
	if since > 0 {
		cutoff = r.nowFn().Add(-since)
	}
	out := make([]LatencyPercentiles, 0, len(r.samples))
	for backend, ring := range r.samples {
		ms := make([]float64, 0, len(ring))
		for _, s := range ring {
			if !cutoff.IsZero() && s.at.Before(cutoff) {
				continue
			}
			ms = append(ms, s.ms)
		}
		sort.Float64s(ms)
		p50, p75, p90, p95, p99 := percentiles(ms)
		out = append(out, LatencyPercentiles{
			Backend: backend,
			Count:   len(ms),
			P50:     p50, P75: p75, P90: p90, P95: p95, P99: p99,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}

// LatencyHistogramHandler serves the admin endpoint.
type LatencyHistogramHandler struct {
	rec *LatencyHistogramRecorder
}

// NewLatencyHistogramHandler returns a handler. A nil recorder
// yields an empty response so the endpoint stays mountable even
// when no retrieval traffic has flowed yet.
func NewLatencyHistogramHandler(rec *LatencyHistogramRecorder) *LatencyHistogramHandler {
	return &LatencyHistogramHandler{rec: rec}
}

// Register mounts the route on rg.
func (h *LatencyHistogramHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/retrieval/latency-histogram", h.get)
}

// LatencyHistogramResponse is the JSON envelope.
type LatencyHistogramResponse struct {
	WindowMinutes int                  `json:"window_minutes"`
	Backends      []LatencyPercentiles `json:"backends"`
}

func (h *LatencyHistogramHandler) get(c *gin.Context) {
	window := 60
	if raw := c.Query("window_minutes"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 60*24 {
			window = v
		}
	}
	rows := h.rec.SinceMinutes(time.Duration(window) * time.Minute)
	c.JSON(http.StatusOK, LatencyHistogramResponse{
		WindowMinutes: window,
		Backends:      rows,
	})
}
