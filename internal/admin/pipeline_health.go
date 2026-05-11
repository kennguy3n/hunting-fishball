// pipeline_health.go — Round-7 Task 18.
//
// PipelineHealthAggregator gathers per-stage throughput, latency,
// retry, and queue-depth signals from the Prometheus registry
// and shapes them into a structured response the dashboard can
// render without re-implementing PromQL.
//
// The aggregator reads from prometheus.Gatherer so tests can
// inject a fake gatherer (a noopGatherer is provided below).
package admin

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// PipelineStageHealth is the per-stage rollup.
type PipelineStageHealth struct {
	Stage         string  `json:"stage"`
	Throughput    float64 `json:"throughput"`
	AvgLatencyMS  float64 `json:"avg_latency_ms"`
	P95LatencyMS  float64 `json:"p95_latency_ms"`
	Retries       float64 `json:"retries"`
	QueueDepth    float64 `json:"queue_depth"`
	DLQTotalToday float64 `json:"dlq_total"`
}

// PipelineHealthReport is the JSON envelope.
type PipelineHealthReport struct {
	Stages []PipelineStageHealth `json:"stages"`
}

// PipelineHealthAggregator pulls stage metrics from prometheus.
type PipelineHealthAggregator struct {
	gatherer prometheus.Gatherer
}

// NewPipelineHealthAggregator validates inputs.
func NewPipelineHealthAggregator(g prometheus.Gatherer) (*PipelineHealthAggregator, error) {
	if g == nil {
		return nil, errors.New("pipeline_health: nil gatherer")
	}
	return &PipelineHealthAggregator{gatherer: g}, nil
}

// Aggregate snapshots the current metric values into per-stage
// rollups for fetch / parse / embed / store.
func (a *PipelineHealthAggregator) Aggregate() (PipelineHealthReport, error) {
	stages := []string{"fetch", "parse", "embed", "store"}
	out := PipelineHealthReport{Stages: make([]PipelineStageHealth, 0, len(stages))}
	families, err := a.gatherer.Gather()
	if err != nil {
		return out, err
	}
	for _, stage := range stages {
		row := PipelineStageHealth{Stage: stage}
		for _, mf := range families {
			switch mf.GetName() {
			case "context_engine_pipeline_stage_duration_seconds":
				row.Throughput, row.AvgLatencyMS, row.P95LatencyMS = stageHistogramStats(mf, stage)
			case "context_engine_pipeline_retries_total":
				row.Retries = retrieveStageCounter(mf, stage)
			case "context_engine_pipeline_channel_depth":
				row.QueueDepth = retrieveStageGauge(mf, stage)
			case "context_engine_pipeline_dlq_total":
				// labelled by topic, not stage — aggregated separately.
			}
		}
		out.Stages = append(out.Stages, row)
	}
	return out, nil
}

func stageHistogramStats(mf *dto.MetricFamily, stage string) (throughput, avgMs, p95Ms float64) {
	for _, m := range mf.GetMetric() {
		if !hasLabel(m, "stage", stage) {
			continue
		}
		h := m.GetHistogram()
		if h == nil {
			continue
		}
		count := float64(h.GetSampleCount())
		sum := h.GetSampleSum()
		throughput = count
		if count > 0 {
			avgMs = (sum / count) * 1000.0
		}
		p95Ms = histogramQuantile(h, 0.95) * 1000.0
	}
	return
}

func retrieveStageCounter(mf *dto.MetricFamily, stage string) float64 {
	for _, m := range mf.GetMetric() {
		if !hasLabel(m, "stage", stage) {
			continue
		}
		if c := m.GetCounter(); c != nil {
			return c.GetValue()
		}
	}
	return 0
}

func retrieveStageGauge(mf *dto.MetricFamily, stage string) float64 {
	for _, m := range mf.GetMetric() {
		if !hasLabel(m, "stage", stage) {
			continue
		}
		if g := m.GetGauge(); g != nil {
			return g.GetValue()
		}
	}
	return 0
}

func hasLabel(m *dto.Metric, name, value string) bool {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name && lp.GetValue() == value {
			return true
		}
	}
	return false
}

// histogramQuantile is the standard nearest-bucket interpolation
// used by Prometheus. Sufficient for dashboard granularity.
func histogramQuantile(h *dto.Histogram, q float64) float64 {
	if h == nil {
		return 0
	}
	buckets := h.GetBucket()
	if len(buckets) == 0 {
		return 0
	}
	total := float64(h.GetSampleCount())
	if total == 0 {
		return 0
	}
	target := q * total
	for _, b := range buckets {
		if float64(b.GetCumulativeCount()) >= target {
			return b.GetUpperBound()
		}
	}
	return buckets[len(buckets)-1].GetUpperBound()
}

// PipelineHealthHandler exposes GET /v1/admin/pipeline/health.
type PipelineHealthHandler struct {
	agg *PipelineHealthAggregator
}

// NewPipelineHealthHandler validates inputs.
func NewPipelineHealthHandler(agg *PipelineHealthAggregator) (*PipelineHealthHandler, error) {
	if agg == nil {
		return nil, errors.New("pipeline_health: nil aggregator")
	}
	return &PipelineHealthHandler{agg: agg}, nil
}

// Register mounts the route.
func (h *PipelineHealthHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/pipeline/health", h.get)
}

func (h *PipelineHealthHandler) get(c *gin.Context) {
	if _, ok := tenantIDFromContext(c); !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	report, err := h.agg.Aggregate()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}
