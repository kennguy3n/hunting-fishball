// Package observability — metrics defines the Prometheus collectors
// used by the API and ingest binaries plus the Python sidecars
// scraped at /metrics.
//
// All collectors live on the package-level Registry, which is exported
// via Handler() so cmd/api and cmd/ingest can mount it under
// `/metrics` without further setup.
package observability

import (
	"log/slog"
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
	// pipeline DLQ observer, broken down by the original topic the
	// message was rerouted from. Operators alert on a non-zero
	// rate of this counter; per-tenant breakdowns live in the
	// structured logs (tenant_id field) where Loki / Splunk index
	// them without the cardinality blow-up that a Prometheus label
	// would carry on a multi-tenant fleet.
	DLQMessagesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_dlq_messages_total",
			Help: "Total dead-letter messages observed on the ingest DLQ topic, by original topic.",
		},
		[]string{"original_topic"},
	)

	// PipelineChannelDepth (Round-6 Task 9) is the current depth of
	// each inter-stage channel in the pipeline. The coordinator
	// records channel len after each submit so operators can spot
	// back-pressure (channel near capacity) before it manifests as
	// rising stage latency.
	PipelineChannelDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "context_engine_pipeline_channel_depth",
			Help: "Current length of each inter-stage pipeline channel.",
		},
		[]string{"stage"},
	)

	// PipelineRetriesTotal (Round-6 Task 19) counts retries per
	// pipeline stage tagged with outcome=retry|exhausted|recovered.
	// Operators alert on a high `exhausted` rate per stage; the
	// `recovered` outcome quantifies how often transient errors
	// resolve on retry.
	PipelineRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_pipeline_retries_total",
			Help: "Pipeline stage retries by stage and outcome.",
		},
		[]string{"stage", "outcome"},
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

// Admin / connector lifecycle metrics — Round-5 additions.
var (
	// TokenRefreshesTotal counts OAuth token refresh attempts the
	// background worker (internal/admin/token_refresh.go) drives.
	// status is one of "success", "skipped", "validation_error",
	// or "transport_error".
	TokenRefreshesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_token_refreshes_total",
			Help: "OAuth token refreshes attempted by the admin worker.",
		},
		[]string{"connector", "status"},
	)
	// CredentialsExpiring is the per-source gauge tracking how
	// many days remain before the active credential's grace period
	// expires. The credential expiry monitor
	// (internal/admin/credential_monitor.go) writes the value once
	// per source per scan; alerting fires when it falls below the
	// configured warning floor.
	CredentialsExpiring = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "context_engine_credentials_expiring_days",
			Help: "Days remaining before the active credential's grace period expires.",
		},
		[]string{"connector", "source_id"},
	)
	// IndexAutoReindexesTotal counts auto-reindex triggers from
	// the watchdog (internal/admin/index_watchdog.go) so SREs can
	// alert on a sudden spike (an upstream backend went unhealthy
	// for more than a handful of tenants).
	IndexAutoReindexesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_index_auto_reindexes_total",
			Help: "Auto-triggered reindex jobs the watchdog scheduled.",
		},
		[]string{"backend", "result"},
	)

	// RetrievalBudgetViolationsTotal counts retrievals whose end-to-
	// end latency exceeded the tenant's configured budget (Round-7
	// Task 11). Operators alert on the cluster-wide rate of this
	// counter; per-tenant breakdowns live in the structured logs
	// (tenant_id field) where Loki / Splunk index them without the
	// cardinality blow-up that a Prometheus label would carry on a
	// multi-tenant fleet — mirroring the DLQMessagesTotal pattern
	// above.
	RetrievalBudgetViolationsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_retrieval_budget_violations_total",
			Help: "Retrievals whose latency exceeded the tenant budget. Per-tenant breakdowns live in structured logs (tenant_id field).",
		},
	)
)

func init() {
	Registry.MustRegister(
		APIRequestsTotal,
		APIRequestDurationSeconds,
		KafkaConsumerLag,
		PipelineStageDurationSeconds,
		DLQMessagesTotal,
		PipelineChannelDepth,
		PipelineRetriesTotal,
		RetrievalBackendDurationSeconds,
		RetrievalBackendHits,
		TokenRefreshesTotal,
		CredentialsExpiring,
		IndexAutoReindexesTotal,
		RetrievalBudgetViolationsTotal,
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
	Registry.Unregister(PipelineChannelDepth)
	Registry.Unregister(PipelineRetriesTotal)
	Registry.Unregister(RetrievalBackendDurationSeconds)
	Registry.Unregister(RetrievalBackendHits)
	Registry.Unregister(TokenRefreshesTotal)
	Registry.Unregister(CredentialsExpiring)
	Registry.Unregister(IndexAutoReindexesTotal)
	Registry.Unregister(RetrievalBudgetViolationsTotal)
	APIRequestsTotal.Reset()
	APIRequestDurationSeconds.Reset()
	KafkaConsumerLag.Reset()
	PipelineStageDurationSeconds.Reset()
	DLQMessagesTotal.Reset()
	PipelineChannelDepth.Reset()
	PipelineRetriesTotal.Reset()
	RetrievalBackendDurationSeconds.Reset()
	RetrievalBackendHits.Reset()
	TokenRefreshesTotal.Reset()
	CredentialsExpiring.Reset()
	IndexAutoReindexesTotal.Reset()
	Registry.MustRegister(
		APIRequestsTotal,
		APIRequestDurationSeconds,
		KafkaConsumerLag,
		PipelineStageDurationSeconds,
		DLQMessagesTotal,
		PipelineChannelDepth,
		PipelineRetriesTotal,
		RetrievalBackendDurationSeconds,
		RetrievalBackendHits,
		TokenRefreshesTotal,
		CredentialsExpiring,
		IndexAutoReindexesTotal,
		RetrievalBudgetViolationsTotal,
	)
}

// ObserveBudgetViolation increments the cluster-wide counter when a
// retrieval response misses its latency budget and emits a
// structured log entry carrying the tenant_id. We deliberately do
// NOT add tenant_id as a Prometheus label — see the comment on
// RetrievalBudgetViolationsTotal and the DLQMessagesTotal precedent.
func ObserveBudgetViolation(tenantID string) {
	RetrievalBudgetViolationsTotal.Inc()
	if tenantID != "" {
		slog.Warn("retrieval budget violation", "tenant_id", tenantID)
	}
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
