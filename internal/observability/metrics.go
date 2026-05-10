// Package observability — metrics defines the Prometheus collectors
// used by the API and ingest binaries plus the Python sidecars
// scraped at /metrics.
//
// All collectors live on the package-level Registry, which is exported
// via Handler() so cmd/api and cmd/ingest can mount it under
// `/metrics` without further setup.
package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the package-level Prometheus registry. Tests reset
// state by unregistering and re-registering the collectors via
// `ResetForTest`.
var Registry = prometheus.NewRegistry()

// API request metrics.
var (
	// APIRequestsTotal counts every Gin-served request, labeled by
	// method, path template, and HTTP status code. Used by the API
	// HPA to scale on QPS.
	APIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_api_requests_total",
			Help: "Total HTTP requests served by the context-engine API.",
		},
		[]string{"method", "path", "status"},
	)
	// APIRequestDurationSeconds tracks API request latency, used to
	// compute p50/p95/p99 SLOs.
	APIRequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "context_engine_api_request_duration_seconds",
			Help:    "API request latency in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
)

// Pipeline (ingest) metrics.
var (
	// KafkaConsumerLag reports the per-partition lag (messages
	// behind the high-water mark). Driven by the consumer's commit
	// loop. Used by the ingest HPA to scale on backlog.
	KafkaConsumerLag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "context_engine_kafka_consumer_lag",
			Help: "Kafka consumer lag (messages behind HWM) per topic/partition.",
		},
		[]string{"topic", "partition", "consumer_group"},
	)
	// PipelineStageDurationSeconds measures Stage 1-4 execution
	// time so operators can spot stage-level regressions.
	PipelineStageDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "context_engine_pipeline_stage_duration_seconds",
			Help:    "Per-stage pipeline execution time (fetch/parse/embed/store).",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"stage"},
	)
	// DLQMessagesTotal counts dead-letter messages observed by the
	// pipeline DLQ observer, broken down by tenant. Operators alert
	// on a non-zero rate of this counter.
	DLQMessagesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_dlq_messages_total",
			Help: "Total dead-letter messages observed on the ingest DLQ topic.",
		},
		[]string{"tenant_id", "original_topic"},
	)
)

// Retrieval metrics.
var (
	// RetrievalBackendDurationSeconds breaks retrieval latency
	// down by backend (vector / bm25 / graph / memory / merge /
	// rerank) so operators can identify the long pole.
	RetrievalBackendDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "context_engine_retrieval_backend_duration_seconds",
			Help:    "Per-backend retrieval latency in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		},
		[]string{"backend"},
	)
	// RetrievalBackendHits is the most-recent hit count per
	// backend; the merge step computes the union/intersection.
	RetrievalBackendHits = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "context_engine_retrieval_backend_hits",
			Help: "Most-recent hit count per retrieval backend.",
		},
		[]string{"backend"},
	)
)

func init() {
	Registry.MustRegister(
		APIRequestsTotal,
		APIRequestDurationSeconds,
		KafkaConsumerLag,
		PipelineStageDurationSeconds,
		DLQMessagesTotal,
		RetrievalBackendDurationSeconds,
		RetrievalBackendHits,
	)
}

// Handler returns the Prometheus HTTP handler bound to the package
// Registry. cmd/api and cmd/ingest mount it at `/metrics`.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

// ResetForTest unregisters and re-registers every collector. Tests
// call this via t.Cleanup to keep state isolated across t.Run cases.
func ResetForTest() {
	Registry.Unregister(APIRequestsTotal)
	Registry.Unregister(APIRequestDurationSeconds)
	Registry.Unregister(KafkaConsumerLag)
	Registry.Unregister(PipelineStageDurationSeconds)
	Registry.Unregister(DLQMessagesTotal)
	Registry.Unregister(RetrievalBackendDurationSeconds)
	Registry.Unregister(RetrievalBackendHits)
	APIRequestsTotal.Reset()
	APIRequestDurationSeconds.Reset()
	KafkaConsumerLag.Reset()
	PipelineStageDurationSeconds.Reset()
	DLQMessagesTotal.Reset()
	RetrievalBackendDurationSeconds.Reset()
	RetrievalBackendHits.Reset()
	Registry.MustRegister(
		APIRequestsTotal,
		APIRequestDurationSeconds,
		KafkaConsumerLag,
		PipelineStageDurationSeconds,
		DLQMessagesTotal,
		RetrievalBackendDurationSeconds,
		RetrievalBackendHits,
	)
}

// ObserveStageDuration records the per-stage duration for the
// given stage label (one of "fetch", "parse", "embed", "store").
func ObserveStageDuration(stage string, seconds float64) {
	PipelineStageDurationSeconds.WithLabelValues(stage).Observe(seconds)
}

// ObserveBackendDuration records the per-backend retrieval
// duration for the given backend label (one of "vector", "bm25",
// "graph", "memory", "merge", "rerank").
func ObserveBackendDuration(backend string, seconds float64) {
	RetrievalBackendDurationSeconds.WithLabelValues(backend).Observe(seconds)
}

// SetBackendHits records the most-recent hit count for a backend.
func SetBackendHits(backend string, hits int) {
	RetrievalBackendHits.WithLabelValues(backend).Set(float64(hits))
}

// SetKafkaConsumerLag records the per-partition lag for a consumer
// group on a topic.
func SetKafkaConsumerLag(topic, partition, group string, lag int64) {
	KafkaConsumerLag.WithLabelValues(topic, partition, group).Set(float64(lag))
}
