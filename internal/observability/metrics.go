// Package observability — metrics defines the Prometheus collectors
// used by the API and ingest binaries plus the Python sidecars
// scraped at /metrics.
//
// All collectors live on the package-level Registry, which is exported
// via Handler() so cmd/api and cmd/ingest can mount it under
// `/metrics` without further setup.
//
// ----------------------------------------------------------------
// Cardinality policy (Round-11 Task 12)
// ----------------------------------------------------------------
//
// Prometheus label values multiply across every dimension. A label
// that takes an unbounded value (notably tenant_id on a multi-tenant
// fleet) explodes the series count and brings down the TSDB. To
// keep cardinality bounded we follow three rules:
//
//  1. NEVER add `tenant_id` as a Prometheus label. Per-tenant
//     breakdowns live in the structured logs (Loki / Splunk index
//     the `tenant_id` slog field without the cardinality blow-up).
//  2. NEVER add user IDs, query strings, or other free-form
//     identifiers as labels.
//  3. Bounded enumerations (stage, backend, status, breaker state)
//     are fine. Adding a new label of this kind must come with a
//     short comment naming the closed enumeration.
//
// The grep-based test in metrics_test.go's
// `TestMetrics_NoTenantIDLabel` pins the first rule mechanically —
// the test fails CI if any registration site lists "tenant_id" in
// its label slice. New metrics with high-cardinality intent should
// route to structured logs instead (see ObserveBudgetViolation for
// the precedent).
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

	// GRPCCircuitBreakerState exposes the current state of each
	// gRPC sidecar circuit breaker — Round-9 Task 10. Values:
	// 0 = closed, 1 = half-open, 2 = open. The `target` label is
	// the gRPC target string (host:port) so operators can alert
	// on any specific sidecar entering open state.
	GRPCCircuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "context_engine_grpc_circuit_breaker_state",
			Help: "Current state of the gRPC sidecar circuit breaker (0=closed, 1=half-open, 2=open).",
		},
		[]string{"target"},
	)

	// PostgresPoolOpenConnections is the Round-9 Task 17 gauge
	// reporting the live `db.Stats().OpenConnections` count for the
	// process-wide Postgres pool. Operators alert on this hitting
	// the pool's MaxOpen ceiling (which signals connection
	// exhaustion downstream of a pgbouncer hiccup or a slow query
	// storm).
	PostgresPoolOpenConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_postgres_pool_open_connections",
			Help: "Current open connections in the Postgres pool (from db.Stats().OpenConnections).",
		},
	)

	// RedisPoolActiveConnections is the Round-9 Task 17 gauge for
	// in-use Redis client connections, read from go-redis's
	// PoolStats().TotalConns - IdleConns. Doubles as a quick
	// signal for cache-warm storms.
	RedisPoolActiveConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_redis_pool_active_connections",
			Help: "Current active (non-idle) connections in the Redis pool.",
		},
	)

	// QdrantPoolIdleConnections is the Round-9 Task 17 gauge for
	// idle HTTP connections held open by the Qdrant client. Idle
	// connections falling to zero under load means the transport
	// is opening fresh sockets for every request and is a hint
	// the keep-alive tunable needs lifting.
	QdrantPoolIdleConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_qdrant_pool_idle_connections",
			Help: "Current idle HTTP connections in the Qdrant client transport.",
		},
	)

	// CacheHitsTotal counts /v1/retrieve requests served from the
	// semantic cache short-circuit (Round-9 Task 16 supporting
	// metric). The cache_hit_rate recording rule divides this
	// counter by (hits + misses) to compute a stable SLI.
	CacheHitsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_retrieval_cache_hits_total",
			Help: "Retrieval requests served from the semantic cache short-circuit.",
		},
	)
	// CacheMissesTotal counts /v1/retrieve requests that consulted
	// the cache and fell through to the full fan-out pipeline. The
	// counter only increments when a cache is actually configured
	// so deployments running without a cache don't inflate the
	// miss series.
	CacheMissesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_retrieval_cache_misses_total",
			Help: "Retrieval requests that consulted the cache and fell through to the fan-out pipeline.",
		},
	)

	// ChunkQualityErrorsTotal counts ChunkQualityRecorder.Record
	// failures observed on the Stage-4 pre-write hook (Round-11
	// Task 4). The hook is best-effort — failures must not block
	// chunk ingestion — but operators need a cluster-wide signal so a
	// silently broken recorder doesn't blank the chunk_quality table
	// without notice. No tenant_id label per the cardinality policy at
	// the top of this file; the slog.Warn emitted alongside carries
	// the tenant_id / chunk_id for triage.
	ChunkQualityErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_chunk_quality_errors_total",
			Help: "ChunkQualityRecorder.Record failures from the Stage-4 pre-write hook. Per-tenant breakdowns live in structured logs.",
		},
	)

	// HookTimeoutsTotal counts coordinator-side Stage-4 hook calls
	// (sync_history Start/Finish, chunk_quality Record) that were
	// dropped because the per-call context.WithTimeout fired before
	// the GORM store wrote (Round-11 Task 5). The `hook` label is a
	// bounded enumeration: sync_history_start / sync_history_finish /
	// chunk_quality_record. The timeout is configurable via
	// CONTEXT_ENGINE_HOOK_TIMEOUT.
	HookTimeoutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_hook_timeouts_total",
			Help: "Stage-4 coordinator hook calls dropped because the per-call context deadline fired.",
		},
		[]string{"hook"},
	)

	// HookPanicsTotal counts coordinator-side Stage-4 hook calls
	// that panicked inside the runWithHookTimeout-spawned goroutine
	// (Round-11 Devin Review Phase-3 follow-up). Distinct from
	// HookTimeoutsTotal so operators can alert separately on a slow
	// store (timeout) versus a crashing store (panic — usually a
	// nil-pointer deref in the GORM driver). The `hook` label uses
	// the same bounded enumeration as HookTimeoutsTotal:
	// sync_history_start / sync_history_finish / chunk_quality_record.
	// The panic itself is converted to a returned error so the
	// ingest process keeps running.
	HookPanicsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_hook_panics_total",
			Help: "Stage-4 coordinator hook calls that panicked inside the spawned goroutine. Distinct from hook_timeouts_total so operators can alert on slow vs crashing stores separately.",
		},
		[]string{"hook"},
	)

	// GORMStoreLookupErrorsTotal counts retrieval-handler GORM store
	// lookup failures observed on the hot path (Round-11 Task 18). The
	// retrieval handler degrades to defaults when its LatencyBudget /
	// CacheTTL / Pin / QueryAnalytics lookups fail; this counter is
	// the operator signal. The `store` label is a bounded enumeration:
	// latency_budget / cache_ttl / pin_lookup / query_analytics.
	GORMStoreLookupErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_gorm_store_lookup_errors_total",
			Help: "GORM-backed lookup failures on the retrieval hot path. The handler degrades to defaults when these fire.",
		},
		[]string{"store"},
	)

	// ChunkQualityScoreAvg is the rolling average chunk quality
	// score across the cluster (Round-11 Task 11). The pipeline
	// updates the gauge after every scoreAndRecordBlocks call —
	// the alert ChunkQualityScoreDropped watches the cluster-wide
	// floor. No tenant_id label per the cardinality policy.
	ChunkQualityScoreAvg = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_chunk_quality_score_avg",
			Help: "Cluster-wide rolling average of ChunkScorer.Score(...).Overall. The ChunkQualityScoreDropped alert fires below 0.5.",
		},
	)

	// CacheHitRate is the rolling cache-hit fraction the retrieval
	// handler updates after every Set/Get (Round-11 Task 11). The
	// alert CacheHitRateLow watches the cluster-wide floor.
	CacheHitRate = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_cache_hit_rate",
			Help: "Rolling cache-hit fraction in [0,1]. The CacheHitRateLow alert fires below 0.3.",
		},
	)

	// CredentialInvalidSources counts how many sources currently
	// have credential_valid=false (Round-11 Task 11). The
	// credential health worker writes this gauge once per scan.
	// Alert CredentialHealthDegraded fires when the gauge is > 0
	// for more than 1h.
	CredentialInvalidSources = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_credential_invalid_sources",
			Help: "Number of sources where credential_valid=false. The CredentialHealthDegraded alert watches a sustained non-zero value.",
		},
	)

	// GORMQueryDuration is the histogram of GORM-backed store query
	// latency (Round-11 Task 11). The `store` label is a bounded
	// enumeration matching the store names in
	// GORMStoreLookupErrorsTotal. Alert GORMStoreLatencyHigh fires
	// when the p95 exceeds 200ms.
	GORMQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "context_engine_gorm_query_duration_seconds",
			Help:    "Wall-clock duration of GORM-backed store queries.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"store"},
	)

	// RetentionExpiredChunksTotal — Round-12 Task 3. Counts chunks
	// expired (and deleted) by the retention worker. The counter
	// only increments when the cross-tier deleter returns success,
	// so failed deletes don't double-bill operators on a sweep that
	// repeatedly retries. No tenant_id label per the cardinality
	// policy at the top of this file; the slog.Info line emitted at
	// sweep boundaries carries the per-tenant counts.
	RetentionExpiredChunksTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_retention_expired_chunks_total",
			Help: "Chunks expired and deleted by the retention worker across all tenants.",
		},
	)

	// RetentionSweepDurationSeconds — Round-12 Task 3. Histogram of
	// the wall-clock duration of a single Sweep call. Operators alert
	// when p95 exceeds the configured interval (default 1h) because
	// that signals the sweep can't keep up with the ingest rate.
	RetentionSweepDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "context_engine_retention_sweep_duration_seconds",
			Help:    "Wall-clock duration of a single retention sweep across all tenants.",
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
		},
	)

	// SchedulerErrorsTotal — Round-12 Task 4. Counter incremented on
	// every recovered panic or returned error from the scheduler
	// tick. Operators alert on a non-zero rate so a silently flapping
	// scheduler can't blackhole sync events for hours. The counter
	// has no labels; per-schedule failures are written to the
	// sync_schedules row (last_error / last_error_at) for triage.
	SchedulerErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_scheduler_errors_total",
			Help: "Scheduler tick failures (panics + returned errors). Per-schedule details live on sync_schedules.last_error.",
		},
	)

	// DLQAutoReplaysTotal — Round-12 Task 6. Counts entries the DLQ
	// auto-replay worker re-emitted to Kafka with capped exponential
	// backoff. The `outcome` label is a bounded enumeration:
	// emitted / skipped / failed.
	DLQAutoReplaysTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_dlq_auto_replays_total",
			Help: "DLQ entries the auto-replay worker re-emitted to Kafka, labelled by outcome.",
		},
		[]string{"outcome"},
	)

	// AdaptiveRateCurrent — Round-12 Task 13. Gauge of the live
	// effective rate (tokens/sec) for each connector's adaptive
	// limiter. The `connector` label is a bounded enumeration (one
	// per registered connector type — currently 12: googledrive,
	// slack, notion, jira, confluence, sharepoint, dropbox, box,
	// onedrive, github, gitlab, teams).
	AdaptiveRateCurrent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "context_engine_adaptive_rate_current",
			Help: "Current effective adaptive rate (tokens/sec) per connector.",
		},
		[]string{"connector"},
	)

	// AdaptiveRateHalvedTotal — Round-12 Task 13. Counter of every
	// halve event (called from AdaptiveRateLimiter.Throttled). A
	// runaway value pages operators because it means an upstream
	// connector is sustaining a 429 storm.
	AdaptiveRateHalvedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_adaptive_rate_halved_total",
			Help: "Adaptive rate halve events per connector.",
		},
		[]string{"connector"},
	)

	// AuditRowsExpiredTotal — Round-12 Task 17. Counts audit_logs
	// rows deleted by the periodic retention sweeper. No labels per
	// the cardinality policy; per-tenant breakdowns live in the
	// slog.Info emitted at sweep completion.
	AuditRowsExpiredTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_audit_rows_expired_total",
			Help: "audit_logs rows deleted by the retention sweeper.",
		},
	)

	// DLQOldestMessageAgeSeconds — Round-13 Task 4. Gauge of the
	// age (in seconds) of the OLDEST unresolved row in dlq_messages
	// — i.e. the row with the smallest failed_at whose replayed_at
	// is still NULL. Operators alert on this exceeding 1h via the
	// DLQAgeHigh alert in deploy/alerts.yaml. The publisher
	// goroutine in cmd/ingest polls every minute.
	DLQOldestMessageAgeSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_dlq_oldest_message_age_seconds",
			Help: "Age in seconds of the oldest unresolved dlq_messages row.",
		},
	)

	// PostgresPoolUtilizationPercent — Round-13 Task 18. Gauge of
	// the API binary's Postgres pool utilisation as a percentage of
	// the configured max (CONTEXT_ENGINE_PG_MAX_OPEN). The leak
	// detector in cmd/api logs a warning when this stays above 90%
	// for three consecutive samples.
	PostgresPoolUtilizationPercent = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "context_engine_postgres_pool_utilization_percent",
			Help: "Postgres pool utilisation as percent of CONTEXT_ENGINE_PG_MAX_OPEN.",
		},
	)

	// StageBreakerStatesTotal — Round-13 Task 5. Counts per-stage
	// circuit-breaker state transitions. Labels: stage (bounded:
	// fetch/parse/embed/store), state (closed/open/half-open).
	StageBreakerStatesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_pipeline_stage_breaker_transitions_total",
			Help: "Per-stage pipeline circuit breaker state transitions.",
		},
		[]string{"stage", "state"},
	)

	// StageBreakerShortCircuitsTotal — Round-13 Task 5. Counts
	// events the per-stage breaker shed straight to the DLQ
	// instead of paying the retry budget.
	StageBreakerShortCircuitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "context_engine_pipeline_stage_breaker_short_circuits_total",
			Help: "Events short-circuited to DLQ by the per-stage circuit breaker.",
		},
		[]string{"stage"},
	)

	// EmbedFallbackTotal — Round-13 Task 19. Counts events that
	// fell back to the Go-native degraded-mode embedder when the
	// gRPC sidecar circuit breaker was open.
	EmbedFallbackTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "context_engine_pipeline_embed_fallback_total",
			Help: "Events that used the degraded-mode embedding fallback.",
		},
	)
)

// SetGRPCCircuitBreakerState records the current breaker state for
// a given gRPC target — Round-9 Task 10.
func SetGRPCCircuitBreakerState(target string, state int) {
	GRPCCircuitBreakerState.WithLabelValues(target).Set(float64(state))
}

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
		GRPCCircuitBreakerState,
		PostgresPoolOpenConnections,
		RedisPoolActiveConnections,
		QdrantPoolIdleConnections,
		CacheHitsTotal,
		CacheMissesTotal,
		ChunkQualityErrorsTotal,
		HookTimeoutsTotal,
		HookPanicsTotal,
		GORMStoreLookupErrorsTotal,
		ChunkQualityScoreAvg,
		CacheHitRate,
		CredentialInvalidSources,
		GORMQueryDuration,
		RetentionExpiredChunksTotal,
		RetentionSweepDurationSeconds,
		SchedulerErrorsTotal,
		DLQAutoReplaysTotal,
		AdaptiveRateCurrent,
		AdaptiveRateHalvedTotal,
		AuditRowsExpiredTotal,
		DLQOldestMessageAgeSeconds,
		PostgresPoolUtilizationPercent,
		StageBreakerStatesTotal,
		StageBreakerShortCircuitsTotal,
		EmbedFallbackTotal,
	)
}

// SetPostgresPoolOpenConnections records the current open
// connection count for the process-wide Postgres pool —
// Round-9 Task 17. Callers wire this from a periodic sampler
// goroutine reading `db.Stats().OpenConnections`.
func SetPostgresPoolOpenConnections(n int) {
	PostgresPoolOpenConnections.Set(float64(n))
}

// SetRedisPoolActiveConnections records the active (non-idle)
// Redis connection count — Round-9 Task 17.
func SetRedisPoolActiveConnections(n int) {
	RedisPoolActiveConnections.Set(float64(n))
}

// SetQdrantPoolIdleConnections records the idle HTTP connection
// count held open by the Qdrant client transport —
// Round-9 Task 17.
func SetQdrantPoolIdleConnections(n int) {
	QdrantPoolIdleConnections.Set(float64(n))
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
	Registry.Unregister(GRPCCircuitBreakerState)
	Registry.Unregister(PostgresPoolOpenConnections)
	Registry.Unregister(RedisPoolActiveConnections)
	Registry.Unregister(QdrantPoolIdleConnections)
	Registry.Unregister(CacheHitsTotal)
	Registry.Unregister(CacheMissesTotal)
	Registry.Unregister(ChunkQualityErrorsTotal)
	Registry.Unregister(HookTimeoutsTotal)
	Registry.Unregister(HookPanicsTotal)
	Registry.Unregister(GORMStoreLookupErrorsTotal)
	Registry.Unregister(ChunkQualityScoreAvg)
	Registry.Unregister(CacheHitRate)
	Registry.Unregister(CredentialInvalidSources)
	Registry.Unregister(GORMQueryDuration)
	Registry.Unregister(RetentionExpiredChunksTotal)
	Registry.Unregister(RetentionSweepDurationSeconds)
	Registry.Unregister(SchedulerErrorsTotal)
	Registry.Unregister(DLQAutoReplaysTotal)
	Registry.Unregister(AdaptiveRateCurrent)
	Registry.Unregister(AdaptiveRateHalvedTotal)
	Registry.Unregister(AuditRowsExpiredTotal)
	Registry.Unregister(DLQOldestMessageAgeSeconds)
	Registry.Unregister(PostgresPoolUtilizationPercent)
	Registry.Unregister(StageBreakerStatesTotal)
	Registry.Unregister(StageBreakerShortCircuitsTotal)
	Registry.Unregister(EmbedFallbackTotal)
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
	GRPCCircuitBreakerState.Reset()
	HookTimeoutsTotal.Reset()
	HookPanicsTotal.Reset()
	GORMStoreLookupErrorsTotal.Reset()
	ChunkQualityScoreAvg.Set(0)
	CacheHitRate.Set(0)
	CredentialInvalidSources.Set(0)
	GORMQueryDuration.Reset()
	PostgresPoolOpenConnections.Set(0)
	RedisPoolActiveConnections.Set(0)
	QdrantPoolIdleConnections.Set(0)
	RetentionSweepDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "context_engine_retention_sweep_duration_seconds",
			Help:    "Wall-clock duration of a single retention sweep across all tenants.",
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
		},
	)
	DLQAutoReplaysTotal.Reset()
	AdaptiveRateCurrent.Reset()
	AdaptiveRateHalvedTotal.Reset()
	// Counters can't be Reset() — unregister + re-register above
	// is enough to drop their accumulated value.
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
		GRPCCircuitBreakerState,
		PostgresPoolOpenConnections,
		RedisPoolActiveConnections,
		QdrantPoolIdleConnections,
		CacheHitsTotal,
		CacheMissesTotal,
		ChunkQualityErrorsTotal,
		HookTimeoutsTotal,
		HookPanicsTotal,
		GORMStoreLookupErrorsTotal,
		ChunkQualityScoreAvg,
		CacheHitRate,
		CredentialInvalidSources,
		GORMQueryDuration,
		RetentionExpiredChunksTotal,
		RetentionSweepDurationSeconds,
		SchedulerErrorsTotal,
		DLQAutoReplaysTotal,
		AdaptiveRateCurrent,
		AdaptiveRateHalvedTotal,
		AuditRowsExpiredTotal,
		DLQOldestMessageAgeSeconds,
		PostgresPoolUtilizationPercent,
		StageBreakerStatesTotal,
		StageBreakerShortCircuitsTotal,
		EmbedFallbackTotal,
	)
}

// ObserveCacheHit increments the retrieval cache hit counter.
// Wired from the /v1/retrieve cache short-circuit (Round-9
// Task 16 supporting metric).
func ObserveCacheHit() {
	CacheHitsTotal.Inc()
}

// ObserveCacheMiss increments the retrieval cache miss counter.
// Only called when a cache is actually configured so deployments
// without a cache don't inflate the miss series.
func ObserveCacheMiss() {
	CacheMissesTotal.Inc()
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

// ObserveChunkQualityError increments the chunk-quality-error
// counter and emits a structured log entry carrying tenant_id and
// chunk_id — Round-11 Task 4. Called by Coordinator.scoreAndRecordBlocks
// on every recorder write failure.
func ObserveChunkQualityError(tenantID, chunkID string, err error) {
	ChunkQualityErrorsTotal.Inc()
	if tenantID != "" || chunkID != "" {
		slog.Warn("chunk quality recorder error", "tenant_id", tenantID, "chunk_id", chunkID, "error", err)
	}
}

// ObserveHookTimeout increments the per-hook timeout counter —
// Round-11 Task 5. `hook` is a bounded enumeration: sync_history_start /
// sync_history_finish / chunk_quality_record.
func ObserveHookTimeout(hook string) {
	HookTimeoutsTotal.WithLabelValues(hook).Inc()
}

// ObserveHookPanic increments the per-hook panic counter —
// Round-11 Devin Review Phase-3 follow-up. Called by
// runWithHookTimeout's recover defer so operators can alert on a
// crashing GORM driver separately from a slow one. `hook` is the
// same bounded enumeration as ObserveHookTimeout.
func ObserveHookPanic(hook string) {
	HookPanicsTotal.WithLabelValues(hook).Inc()
}

// ObserveGORMStoreLookupError increments the per-store lookup
// error counter — Round-11 Task 18. `store` is a bounded
// enumeration: latency_budget / cache_ttl / pin_lookup /
// query_analytics.
func ObserveGORMStoreLookupError(store string) {
	GORMStoreLookupErrorsTotal.WithLabelValues(store).Inc()
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
