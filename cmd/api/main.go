// Command context-engine-api is the Gin retrieval API binary.
//
// Phase 1 wiring:
//   - Connects to PostgreSQL via GORM (CONTEXT_ENGINE_DATABASE_URL).
//   - Connects to Qdrant via REST (CONTEXT_ENGINE_QDRANT_URL).
//   - Connects to the embedding gRPC service for query embedding
//     (CONTEXT_ENGINE_EMBEDDING_TARGET).
//
// Phase 3 wiring (all optional — empty env var skips the backend):
//   - Tantivy/bleve BM25 (CONTEXT_ENGINE_BM25_DIR).
//   - FalkorDB graph + Redis semantic cache, both off the same
//     go-redis pool (CONTEXT_ENGINE_REDIS_URL,
//     CONTEXT_ENGINE_FALKOR_ENABLED).
//   - Mem0 gRPC client (CONTEXT_ENGINE_MEMORY_TARGET).
//
// Common:
//   - Mounts the audit-log handler under the auth middleware group.
//   - Mounts the retrieval handler under the auth middleware group.
//   - Listens on CONTEXT_ENGINE_LISTEN_ADDR (default :8080).
//
// The auth middleware itself is a placeholder until the OIDC integration
// from the platform repo lands; for now it accepts a tenant ID via the
// X-Tenant-ID header. Callers behind the BFF (auth-tier) provide a real
// tenant ID; direct calls without the header receive 401.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/b2c"
	"github.com/kennguy3n/hunting-fishball/internal/config"
	errcat "github.com/kennguy3n/hunting-fishball/internal/errors"
	"github.com/kennguy3n/hunting-fishball/internal/eval"
	"github.com/kennguy3n/hunting-fishball/internal/lifecycle"
	"github.com/kennguy3n/hunting-fishball/internal/migrate"
	"github.com/kennguy3n/hunting-fishball/internal/models"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
	memoryv1 "github.com/kennguy3n/hunting-fishball/proto/memory/v1"

	// Phase 7 connector blank-imports — the registry's init()
	// hooks register each implementation under the global registry
	// so admin source-management can mint Connection objects from
	// the connector name alone.
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/asana"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/box"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/clickup"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/confluence_server"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/discord"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/github"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gmail"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/jira"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/linear"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/mattermost"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/monday"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/notion"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/okta"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/pipedrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/rss"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/s3"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/salesforce"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/teams"
)

func main() {
	// Phase 8 Task 17: structured JSON logging. Every log line emitted
	// from this binary inherits component="api" and (when the request
	// context carries them) tenant_id and trace_id, so log shippers
	// can correlate entries without parsing free-form prefixes.
	logger := observability.NewLogger("api")
	observability.SetDefault(logger)
	slog.SetDefault(logger)
	if err := run(); err != nil {
		logger.Error("api: fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	if err := config.ValidateAPI(config.OSLooker); err != nil {
		return err
	}
	listenAddr := envOr("CONTEXT_ENGINE_LISTEN_ADDR", ":8080")
	dsn := envOr("CONTEXT_ENGINE_DATABASE_URL", "")
	qdrantURL := envOr("CONTEXT_ENGINE_QDRANT_URL", "http://localhost:6333")
	embedTarget := envOr("CONTEXT_ENGINE_EMBEDDING_TARGET", "localhost:8081")
	vectorSizeStr := envOr("CONTEXT_ENGINE_VECTOR_SIZE", "1536")
	vectorSize, err := parseInt(vectorSizeStr)
	if err != nil {
		return fmt.Errorf("CONTEXT_ENGINE_VECTOR_SIZE: %w", err)
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	// Phase 8 / Task 18: optional automatic SQL migration runner.
	// Off by default. Callers set AUTO_MIGRATE=true (or CONTEXT_ENGINE_AUTO_MIGRATE=true)
	// to apply every pending file in MIGRATIONS_DIR (default "migrations") at startup.
	if isAutoMigrateEnabled() {
		dir := envOr("CONTEXT_ENGINE_MIGRATIONS_DIR", "migrations")
		runner, mrr := migrate.New(migrate.Config{DB: db, Dir: dir})
		if mrr != nil {
			return fmt.Errorf("migrate: %w", mrr)
		}
		applied, mrr := runner.Apply(context.Background())
		if mrr != nil {
			return fmt.Errorf("auto-migrate: %w", mrr)
		}
		slog.Info("api: applied migrations", slog.Int("count", len(applied)))
	}
	// Phase 8 Task 18: configurable Postgres pool sizes. Defaults are
	// chosen for the per-tenant write rate observed in the Phase 1
	// load tests; callers tune via env in capacity-test runs.
	if sqlDB, derr := db.DB(); derr == nil {
		maxOpen := 32
		if v, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_PG_MAX_OPEN")); v > 0 {
			maxOpen = v
		}
		maxIdle := 16
		if v, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_PG_MAX_IDLE")); v > 0 {
			maxIdle = v
		}
		sqlDB.SetMaxOpenConns(maxOpen)
		sqlDB.SetMaxIdleConns(maxIdle)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}

	auditRepo := audit.NewRepository(db)

	// Round-8 Task 5: notification dispatcher wiring. Build the
	// dispatcher + delivery log here so every handler that takes
	// an AuditWriter can be wrapped in a NotifyingAuditWriter,
	// which fans configured webhook/email subscribers out on every
	// audit Create. Failures inside Dispatch are swallowed so they
	// can never block the audit write.
	// Round-9 Task 1: GORM-backed NotificationStore. The store
	// auto-migrates so the notification_preferences table is created
	// even if the SQL migration hasn't been replayed.
	notifStore, err := admin.NewNotificationStoreGORM(db)
	if err != nil {
		return fmt.Errorf("notification store: %w", err)
	}
	if err := notifStore.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("notification_preferences migrate: %w", err)
	}
	notifDelivery := &admin.WebhookDelivery{Client: http.DefaultClient}
	notifDispatcher, err := admin.NewNotificationDispatcher(notifStore, notifDelivery)
	if err != nil {
		return fmt.Errorf("notification dispatcher: %w", err)
	}
	notifDeliveryLog := admin.NewNotificationDeliveryLogGORM(db)
	if err := notifDeliveryLog.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("notif_delivery_log migrate: %w", err)
	}
	notifDispatcher.SetDeliveryLog(notifDeliveryLog)
	notifyingAudit := &admin.NotifyingAuditWriter{Inner: auditRepo, Dispatcher: notifDispatcher, Async: true}

	// Round-8 Task 17 finisher: start the NotificationRetryWorker
	// in a background goroutine. Without this the dispatcher writes
	// next_retry_at on retryable failures but nothing scans the log
	// to re-attempt delivery, so failed webhooks would simply rot.
	// The interval is configurable via the documented
	// CONTEXT_ENGINE_NOTIFICATION_RETRY_INTERVAL env var; default 1m.
	notifRetryWorker, err := admin.NewNotificationRetryWorker(admin.NotificationRetryWorkerConfig{
		Store:    notifDeliveryLog,
		Delivery: notifDelivery,
		Logger: func(msg string, kv ...any) {
			slog.Warn("notification_retry", slog.String("msg", msg), slog.Any("kv", kv))
		},
	})
	if err != nil {
		return fmt.Errorf("notification retry worker: %w", err)
	}
	notifRetryInterval := time.Minute
	if v := os.Getenv("CONTEXT_ENGINE_NOTIFICATION_RETRY_INTERVAL"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
			notifRetryInterval = d
		}
	}
	notifRetryCtx, notifRetryCancel := context.WithCancel(context.Background())
	defer notifRetryCancel()
	go func() {
		t := time.NewTicker(notifRetryInterval)
		defer t.Stop()
		for {
			select {
			case <-notifRetryCtx.Done():
				return
			case <-t.C:
				notifRetryWorker.Tick(notifRetryCtx)
			}
		}
	}()

	qdrantPool := 32
	if v, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_QDRANT_POOL_SIZE")); v > 0 {
		qdrantPool = v
	}
	qdrant, err := storage.NewQdrantClient(storage.QdrantConfig{
		BaseURL:                 qdrantURL,
		VectorSize:              vectorSize,
		PoolMaxIdleConnsPerHost: qdrantPool,
	})
	if err != nil {
		return fmt.Errorf("qdrant: %w", err)
	}

	conn, err := grpc.NewClient(embedTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial embed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	embedder, err := pipeline.NewEmbedder(pipeline.EmbedConfig{
		Local: embeddingv1.NewEmbeddingServiceClient(conn),
	})
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	queryEmbedder := &queryEmbedAdapter{e: embedder}

	// LiveResolverGORM reads the live policy tables LiveStoreGORM
	// writes (tenant_policies, channel_policies, policy_acl_rules,
	// recipient_policies). The retrieval handler consults it on
	// every request; the Phase 4 simulator uses the SAME instance
	// as its LiveResolver so what-if diffs are computed against
	// the freshest live state.
	liveResolver := policy.NewLiveResolverGORM(db)

	// Phase 6 / Task 15: shard-version lookup feeds the device-first
	// hint. The shard repository is constructed once here and reused
	// by both the retrieval handler (read-only LatestVersion lookups)
	// and the shard handler (full read/write surface) further down.
	shardRepo := shard.NewRepository(db)

	handlerCfg := retrieval.HandlerConfig{
		VectorStore:        qdrant,
		Embedder:           queryEmbedder,
		PolicyResolver:     liveResolver,
		ShardVersionLookup: shard.VersionLookup{Repo: shardRepo},
	}
	// Round-13 Task 8: slow-query threshold. Defaults to 1000ms.
	// Setting it to 0 (or any non-numeric value) disables the
	// slow-query flag + log.
	slowMS := 1000
	if raw := os.Getenv("CONTEXT_ENGINE_SLOW_QUERY_THRESHOLD_MS"); raw != "" {
		if v, perr := strconv.Atoi(raw); perr == nil && v >= 0 {
			slowMS = v
		}
	}
	handlerCfg.SlowQueryThresholdMS = slowMS

	// Phase 3: BM25, graph, semantic cache, Mem0. Each backend is
	// optional — when its env var is empty the handler skips it and
	// falls back to the Phase 1 single-stream behaviour.
	if dir := os.Getenv("CONTEXT_ENGINE_BM25_DIR"); dir != "" {
		bm, err := storage.NewTantivyClient(storage.TantivyConfig{RootDir: dir})
		if err != nil {
			return fmt.Errorf("tantivy: %w", err)
		}
		handlerCfg.BM25 = &retrieval.TantivyAdapter{Client: bm}
	}
	var sharedRedis *redis.Client
	// sharedSemanticCache is the concrete *storage.SemanticCache
	// retained at construction time so Round-10 Task 2's
	// SetTenantTTLLookup hook can be attached after the admin GORM
	// store is built. handlerCfg.Cache stores the same value
	// through the retrieval.Cache interface.
	var sharedSemanticCache *storage.SemanticCache
	if redisURL := os.Getenv("CONTEXT_ENGINE_REDIS_URL"); redisURL != "" {
		opts, perr := redis.ParseURL(redisURL)
		if perr != nil {
			return fmt.Errorf("redis url: %w", perr)
		}
		// Phase 8 Task 18: configurable connection pool size for the
		// Redis-backed semantic cache + FalkorDB adapter.
		if v, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_REDIS_POOL_SIZE")); v > 0 {
			opts.PoolSize = v
		}
		rc := redis.NewClient(opts)
		sharedRedis = rc
		semCache, scErr := storage.NewSemanticCache(storage.SemanticCacheConfig{
			Client:    rc,
			KeyPrefix: "hf:",
		})
		if scErr != nil {
			return fmt.Errorf("semantic cache: %w", scErr)
		}
		handlerCfg.Cache = semCache
		sharedSemanticCache = semCache
		if os.Getenv("CONTEXT_ENGINE_FALKOR_ENABLED") == "1" {
			falkor, ferr := storage.NewFalkorDBClient(storage.FalkorDBConfig{
				Client: storage.FalkorDBFromRedis(rc),
			})
			if ferr != nil {
				return fmt.Errorf("falkordb: %w", ferr)
			}
			handlerCfg.Graph = &retrieval.FalkorAdapter{Client: falkor}
		}
	}
	if memTarget := os.Getenv("CONTEXT_ENGINE_MEMORY_TARGET"); memTarget != "" {
		memConn, merr := grpc.NewClient(memTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if merr != nil {
			return fmt.Errorf("dial memory: %w", merr)
		}
		defer func() { _ = memConn.Close() }()
		handlerCfg.Memory = &memoryAdapter{c: memoryv1.NewMemoryServiceClient(memConn)}
	}

	// Round-9 Task 9: optional async cache warm-on-miss.
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXT_ENGINE_CACHE_WARM_ON_MISS"))); v == "1" || v == "true" {
		handlerCfg.CacheWarmOnMiss = true
	}

	retrievalHandler, err := retrieval.NewHandler(handlerCfg)
	if err != nil {
		return fmt.Errorf("retrieval handler: %w", err)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	// Phase 8 / Task 20: request ID middleware mounts ahead of
	// everything else so every downstream log line, response
	// header, and trace span carries the X-Request-ID.
	r.Use(observability.RequestIDMiddleware())
	r.Use(observability.PrometheusMiddleware())
	// Round-13 Task 11: payload size limiter. Configurable via
	// CONTEXT_ENGINE_MAX_REQUEST_BODY_BYTES; defaults to 10 MiB.
	maxBody := observability.DefaultMaxRequestBodyBytes
	if raw := os.Getenv("CONTEXT_ENGINE_MAX_REQUEST_BODY_BYTES"); raw != "" {
		if v, perr := strconv.ParseInt(raw, 10, 64); perr == nil && v > 0 {
			maxBody = v
		}
	}
	r.Use(observability.PayloadSizeLimiter(observability.PayloadLimiterConfig{
		MaxBytes:  maxBody,
		SkipPaths: []string{"/metrics", "/healthz", "/readyz"},
	}))
	// Round-4 Task 7: structured error envelope. The middleware
	// converts any *errors.Error attached via c.Error() into a
	// JSON envelope keyed by stable codes so log scanners and
	// SDK clients can branch on machine-readable identifiers
	// rather than HTTP status codes alone.
	r.Use(errcat.Middleware())
	r.GET("/metrics", gin.WrapH(observability.Handler()))
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/readyz", apiReadyzHandler(db, sharedRedis, qdrant))

	apiMiddlewares := []gin.HandlerFunc{authPlaceholder, observability.GinLoggerMiddleware("api"), observability.APIVersionMiddleware(observability.APIVersionConfig{})}

	// Phase 8 / Task 8: optional public-API rate limit. Active when
	// CONTEXT_ENGINE_API_RATE_LIMIT (requests-per-second per tenant)
	// is set and Redis is wired. The limiter falls open on Redis
	// failures so a transient cache outage doesn't blackhole the API.
	if sharedRedis != nil {
		if rps, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_API_RATE_LIMIT")); rps > 0 {
			burst := rps * 4
			if v, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_API_RATE_BURST")); v > 0 {
				burst = v
			}
			apiLimiter, lerr := admin.NewRateLimiter(context.Background(), admin.RateLimiterConfig{
				Client:    sharedRedis,
				KeyPrefix: "hf:rl:api",
				Limit:     admin.RateLimit{Capacity: burst, RefillPerSecond: float64(rps)},
			})
			if lerr != nil {
				return fmt.Errorf("api rate limiter: %w", lerr)
			}
			mw, merr := admin.NewAPIRateLimitMiddleware(admin.APIRateLimitMiddlewareConfig{Limiter: apiLimiter})
			if merr != nil {
				return fmt.Errorf("api rate limit middleware: %w", merr)
			}
			apiMiddlewares = append(apiMiddlewares, mw)
		}
	}

	api := r.Group("/", apiMiddlewares...)
	audit.NewHandler(auditRepo).Register(api)
	// Round-13 Task 12: audit integrity endpoint.
	if integ, ierr := audit.NewIntegrityHandler(auditRepo); ierr == nil {
		integ.Register(api)
	}
	retrievalHandler.Register(api)

	// Admin source-management surface (Phase 2). The handler mounts
	// POST/GET/PATCH/DELETE under /v1/admin/sources and tolerates a
	// nil PolicyResolver / EventEmitter for the bare-bones config —
	// production wiring fills those in.
	sourceRepo := admin.NewSourceRepository(db)
	healthRepo := admin.NewHealthRepository(db, admin.DefaultThresholds)
	adminHandler, err := admin.NewHandler(admin.HandlerConfig{
		Repo:   sourceRepo,
		Audit:  notifyingAudit,
		Health: healthRepo,
	})
	if err != nil {
		return fmt.Errorf("admin handler: %w", err)
	}
	adminHandler.Register(api)

	// Round-4 Task 17: tenant usage metering endpoint. Maintains
	// the tenant_usage rollup table and exposes
	// GET /v1/admin/tenants/:id/usage. The store is also held by
	// the API binary for hot-path increment counters.
	meteringStore := admin.NewMeteringStoreGORM(db)
	if err := meteringStore.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("tenant_usage migrate: %w", err)
	}
	admin.NewMeteringHandler(meteringStore).Register(api)

	// Round-4 Task 16: connector credential rotation endpoint.
	// Mounts POST /v1/admin/sources/:id/rotate-credentials. The
	// rotator validates new credentials via the connector's
	// Validate before swapping; the previous credential is kept
	// for CredentialGracePeriod so in-flight requests drain.
	(&admin.CredentialRotator{
		Repo:      sourceRepo,
		Audit:     notifyingAudit,
		Validator: admin.NewRegistryValidator(),
	}).Register(api)

	// Round-4 Task 5: per-source cron scheduler endpoints.
	// Mounts POST/GET/DELETE /v1/admin/sources/:id/schedule. The
	// scheduler goroutine itself runs in cmd/ingest; the API
	// surface only manages the schedule rows.
	if err := db.AutoMigrate(&admin.SyncSchedule{}); err != nil {
		return fmt.Errorf("sync_schedules migrate: %w", err)
	}
	admin.NewSchedulerHandler(db).Register(api)

	// Phase 8 / Task 19: connector / pipeline / retrieval health
	// dashboard endpoint. Metrics provider is optional — when nil the
	// handler returns just connector health.
	dashboardHandler, err := admin.NewDashboardHandler(healthRepo, nil)
	if err != nil {
		return fmt.Errorf("dashboard handler: %w", err)
	}
	dashboardHandler.Register(api)

	// Phase 8 / Task 14: per-namespace sync progress endpoint.
	syncProgressStore := admin.NewSyncProgressStoreGORM(db)
	if err := syncProgressStore.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("sync_progress migrate: %w", err)
	}
	admin.NewSyncProgressHandler(syncProgressStore).Register(api)

	// Round-4 Task 19: per-backend index health endpoint.
	// Mounts GET /v1/admin/health/indexes — one Ping per backend
	// in parallel, returns 200 when all green and 503 when any
	// backend is unhealthy. The DB checker is the GORM connection
	// itself; vector + cache pings are wired off the existing
	// clients.
	indexCheckers := []admin.BackendChecker{
		admin.PingChecker{
			BackendName: "postgres",
			PingFn: func(ctx context.Context) error {
				sqlDB, err := db.DB()
				if err != nil {
					return err
				}
				return sqlDB.PingContext(ctx)
			},
		},
		admin.PingChecker{BackendName: "qdrant", PingFn: qdrant.Ping},
	}
	if sharedRedis != nil {
		indexCheckers = append(indexCheckers, admin.PingChecker{
			BackendName: "redis",
			PingFn: func(ctx context.Context) error {
				return sharedRedis.Ping(ctx).Err()
			},
		})
	}
	admin.NewIndexHealthHandler(indexCheckers...).Register(api)

	// Round-13 Task 1: structured health-check aggregator.
	// GET /v1/admin/health/summary fans every backing probe out
	// in parallel and returns a verdict (healthy/degraded/unhealthy)
	// with per-component latency. Probes reuse the same closures
	// the per-backend health endpoint runs so the two surfaces
	// can't drift.
	summaryProbes := []admin.SummaryProbe{
		admin.SummaryProbeFunc{ProbeName: "postgres", Fn: func(ctx context.Context) (admin.SummaryStatus, error) {
			sqlDB, err := db.DB()
			if err != nil {
				return admin.SummaryStatusUnhealthy, err
			}
			if err := sqlDB.PingContext(ctx); err != nil {
				return admin.SummaryStatusUnhealthy, err
			}
			return admin.SummaryStatusHealthy, nil
		}},
		admin.SummaryProbeFunc{ProbeName: "qdrant", Fn: func(ctx context.Context) (admin.SummaryStatus, error) {
			if err := qdrant.Ping(ctx); err != nil {
				return admin.SummaryStatusUnhealthy, err
			}
			return admin.SummaryStatusHealthy, nil
		}},
	}
	if sharedRedis != nil {
		summaryProbes = append(summaryProbes, admin.SummaryProbeFunc{ProbeName: "redis", Fn: func(ctx context.Context) (admin.SummaryStatus, error) {
			if err := sharedRedis.Ping(ctx).Err(); err != nil {
				return admin.SummaryStatusUnhealthy, err
			}
			return admin.SummaryStatusHealthy, nil
		}})
	}
	admin.NewHealthSummaryHandler(summaryProbes...).Register(api)

	// Round-4 Task 18: server-sent-events feed for live sync progress.
	// Mounts GET /v1/admin/sources/:id/sync/stream and pushes
	// discovered / processed / failed / completed events as the
	// per-namespace counters change. Polls the same store as the
	// snapshot endpoint at StreamPollInterval (5s).
	admin.NewProgressStreamHandler(syncProgressStore).Register(api)

	// Phase 8 / Task 7: optional reindex admin surface. Wires a Kafka
	// SyncProducer into a pipeline.ReindexOrchestrator so an admin
	// can re-emit EventReindex envelopes for an entire (tenant_id,
	// source_id [, namespace_id]) without re-fetching from the upstream
	// source. Off by default; set CONTEXT_ENGINE_KAFKA_BROKERS to opt
	// the API binary into emitting reindex events.
	if brokers := os.Getenv("CONTEXT_ENGINE_KAFKA_BROKERS"); brokers != "" {
		topic := envOr("CONTEXT_ENGINE_REINDEX_TOPIC", "ingest")
		brokerList := strings.Split(brokers, ",")
		saramaCfg := pipeline.SaramaConfig()
		reindexProd, perr := sarama.NewSyncProducer(brokerList, saramaCfg)
		if perr != nil {
			return fmt.Errorf("kafka reindex producer: %w", perr)
		}
		defer func() { _ = reindexProd.Close() }()
		producer, err := pipeline.NewProducer(pipeline.ProducerConfig{Producer: reindexProd, Topic: topic})
		if err != nil {
			return fmt.Errorf("reindex producer: %w", err)
		}
		enum, err := pipeline.NewPostgresReindexEnumerator(db)
		if err != nil {
			return fmt.Errorf("reindex enumerator: %w", err)
		}
		orch, err := pipeline.NewReindexOrchestrator(enum, producer)
		if err != nil {
			return fmt.Errorf("reindex orchestrator: %w", err)
		}
		reindexHandler, err := admin.NewReindexHandler(admin.ReindexHandlerConfig{Runner: orch, Audit: notifyingAudit})
		if err != nil {
			return fmt.Errorf("reindex handler: %w", err)
		}
		reindexHandler.Register(api)
	}

	// Phase 5/6: tenant-scope key destruction (cryptographic-forget)
	// admin endpoint. Mounted on /v1/admin/tenants/:tenant_id; the
	// per-channel forget is wired separately under /v1/tenants/:id/keys
	// inside the shard handler. The deleter starts with no sweepers
	// because production sweep wiring lives in cmd/ingest's binary;
	// this binary owns the read+admin surface only.
	tenantStatusRepo := admin.NewTenantStatusRepoGORM(db)
	tenantDeleter, err := admin.NewTenantDeleter(admin.TenantDeleterConfig{
		Status: tenantStatusRepo,
		Audit:  notifyingAudit,
	})
	if err != nil {
		return fmt.Errorf("tenant deleter: %w", err)
	}
	tenantDeleteHandler, err := admin.NewTenantDeleteHandler(tenantDeleter)
	if err != nil {
		return fmt.Errorf("tenant delete handler: %w", err)
	}
	tenantDeleteHandler.Register(api)

	// Phase 4 simulator + draft policy surface. The Promoter wires
	// the GORM-backed live store (writes the live policy tables in
	// migrations/004_policy.sql) to the draft repo so promote =
	// "make live look exactly like this draft, atomically".
	//
	// LiveResolver and Retriever are both real now: the resolver
	// reads the same tables LiveStoreGORM writes, and the
	// Retriever delegates to retrievalHandler.RetrieveWithSnapshot
	// so what-if diffs run the full fan-out + merge + rerank +
	// ACL/recipient gate against the supplied snapshot. The
	// snapshot-driven retrieval bypasses the semantic cache by
	// design — see the method's docstring.
	draftRepo := policy.NewDraftRepository(db)
	liveStore := policy.NewLiveStoreGORM(db)
	promoter, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts:    draftRepo,
		LiveStore: liveStore,
		Audit:     notifyingAudit,
	})
	if err != nil {
		return fmt.Errorf("policy promoter: %w", err)
	}
	simulator, err := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: liveResolver,
		Retriever:    simulatorRetriever(retrievalHandler),
	})
	if err != nil {
		return fmt.Errorf("policy simulator: %w", err)
	}
	simHandler, err := admin.NewSimulatorHandler(admin.SimulatorConfig{
		Drafts:    draftRepo,
		Promotion: promoter,
		Simulator: simulator,
		Audit:     notifyingAudit,
	})
	if err != nil {
		return fmt.Errorf("simulator handler: %w", err)
	}
	simHandler.Register(api)

	// Phase 6 / Tasks 1: retrieval evaluation harness. Mounts
	// /v1/admin/eval/{suites,run} so operators can replay a labelled
	// corpus against the live retrieval handler and score
	// Precision@K, Recall@K, MRR, NDCG. The retriever closure
	// adapts retrievalHandler.Retrieve into the eval package's
	// neutral request/hit shape so eval stays a leaf package.
	evalSuiteRepo := eval.NewSuiteRepository(db)
	evalHandler, err := eval.NewHandler(eval.HandlerConfig{
		Suites:   evalSuiteRepo,
		Audit:    auditRepo,
		Retrieve: evalRetriever(retrievalHandler, liveResolver),
	})
	if err != nil {
		return fmt.Errorf("eval handler: %w", err)
	}
	evalHandler.Register(api)

	// Phase 5 server-side: shard sync API. The handler exposes
	// `GET /v1/shards/:tenant_id`, `GET /v1/shards/:tenant_id/delta`,
	// `GET /v1/shards/:tenant_id/coverage`, and (when a Forget
	// orchestrator is wired) `DELETE /v1/tenants/:tenant_id/keys`. The
	// actual sweepers (Qdrant, FalkorDB, Tantivy, Redis, Postgres) are
	// tier-specific and live under cmd/ingest's wiring; the API
	// binary only needs the read path to be live.
	// CoverageRepoGORM counts the corpus-side chunk rows so the
	// `/v1/shards/:tenant_id/coverage` endpoint can report
	// IsAuthoritative=true (numerator/denominator both observable);
	// without this wiring the endpoint would forever return
	// reason=corpus_size_unknown.
	shardCoverageRepo := shard.NewCoverageRepoGORM(db)
	shardHandlerCfg := shard.HandlerConfig{Repo: shardRepo, CoverageRepo: shardCoverageRepo}
	shardHandler, err := shard.NewHandler(shardHandlerCfg)
	if err != nil {
		return fmt.Errorf("shard handler: %w", err)
	}
	shardHandler.Register(api)

	// Phase 5 / Task 13: model catalog. The static provider lists
	// the Bonsai-1.7B builds the on-device runtime is allowed to
	// download, alongside the per-tier eviction policy that drives
	// shard retention on-device.
	modelHandler, err := models.NewHandler(models.NewStaticCatalog())
	if err != nil {
		return fmt.Errorf("models handler: %w", err)
	}
	modelHandler.Register(api)

	// Round-6 + Round-7 admin handlers. Each handler defaults to
	// an in-memory store when there's no GORM-backed equivalent
	// in production yet, so the API binary boots without external
	// dependencies while still exposing the full surface for
	// integration tests.
	// Round-9 Task 2: GORM-backed ABTestStore. Both the admin
	// handler and the retrieval ABTestRouter share the same store
	// so writes by operators are immediately visible to the
	// retrieval fan-out.
	var abtestStore admin.ABTestStore
	if gormStore, gerr := admin.NewABTestStoreGORM(db); gerr == nil {
		if merr := gormStore.AutoMigrate(context.Background()); merr != nil {
			return fmt.Errorf("retrieval_ab_tests migrate: %w", merr)
		}
		abtestStore = gormStore
	} else {
		return fmt.Errorf("abtest store: %w", gerr)
	}
	if abtestHandler, aerr := admin.NewABTestHandler(abtestStore, notifyingAudit); aerr == nil {
		abtestHandler.Register(api)
	}
	abtestRouter := admin.NewABTestRouter(abtestStore)
	retrievalHandler.SetABTestRouter(retrieval.ABTestRouterAdapter{Router: abtestRouter})

	// Round-7 Task 4 + Task 6: the A/B-test results aggregator must
	// read from the *same* query-analytics store the recorder writes
	// to, otherwise GET /v1/admin/retrieval/experiments/:name/results
	// always returns zero arms. Hoist the store construction here so
	// the aggregator and the recorder share one instance.
	//
	// Round-8 Task 6: GORM-backed. The store auto-migrates so the
	// query_analytics table is created even if the SQL migration
	// hasn't been replayed against the target database.
	queryAnalyticsStore := admin.NewQueryAnalyticsStoreGORM(db)
	if err := queryAnalyticsStore.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("query_analytics migrate: %w", err)
	}

	if abtestResults, aerr := admin.NewABTestResultsAggregator(queryAnalyticsStore); aerr == nil {
		if arHandler, herr := admin.NewABTestResultsHandler(abtestResults); herr == nil {
			arHandler.Register(api)
		}
	}

	// Round-9 Task 3: GORM-backed ConnectorTemplateStore.
	if tmplStore, terr := admin.NewConnectorTemplateStoreGORM(db); terr == nil {
		if merr := tmplStore.AutoMigrate(context.Background()); merr != nil {
			return fmt.Errorf("connector_templates migrate: %w", merr)
		}
		if tmplHandler, herr := admin.NewConnectorTemplateHandler(tmplStore, notifyingAudit); herr == nil {
			tmplHandler.Register(api)
		}
	} else {
		return fmt.Errorf("connector_template store: %w", terr)
	}

	if notifHandler, nerr := admin.NewNotificationHandler(notifStore, notifyingAudit); nerr == nil {
		notifHandler.Register(api)
	}
	if dlogHandler, derr := admin.NewNotificationDeliveryLogHandler(notifDeliveryLog); derr == nil {
		dlogHandler.Register(api)
	}

	if ecHandler, eerr := admin.NewEmbeddingConfigHandler(admin.NewEmbeddingConfigRepository(db), notifyingAudit); eerr == nil {
		ecHandler.Register(api)
	}

	if rsHandler, rerr := admin.NewRetryStatsHandler(pipeline.NewRetryAnalytics()); rerr == nil {
		rsHandler.Register(api)
	}

	// Round-9 Task 4: GORM-backed SynonymStore.
	synStore, serr := retrieval.NewSynonymStoreGORM(db)
	if serr != nil {
		return fmt.Errorf("synonym store: %w", serr)
	}
	if merr := synStore.AutoMigrate(context.Background()); merr != nil {
		return fmt.Errorf("retrieval_synonyms migrate: %w", merr)
	}
	if synHandler, herr := admin.NewSynonymsHandler(synStore, notifyingAudit); herr == nil {
		synHandler.Register(api)
	}
	// Round-10 Task 9: wire the SynonymExpander so retrieve
	// requests fan out through the tenant's expanded query. The
	// 4-token append cap matches the in-memory expander default.
	retrievalHandler.SetQueryExpander(retrieval.NewSynonymExpander(synStore, 4))

	// Round-7 Task 4: retrieval query analytics. The retrieval
	// handler exposes a function-shaped hook to keep the import
	// graph one-way (admin imports retrieval, never reverse). The
	// `queryAnalyticsStore` is declared above so the A/B-test
	// results aggregator shares the same in-memory backing store.
	queryAnalyticsRec, _ := admin.NewQueryAnalyticsRecorder(queryAnalyticsStore)
	retrievalHandler.SetQueryAnalyticsRecorder(func(ctx context.Context, e retrieval.QueryAnalyticsEvent) {
		queryAnalyticsRec.Record(ctx, admin.QueryAnalyticsEvent{
			TenantID: e.TenantID, QueryText: e.QueryText, TopK: e.TopK,
			HitCount: e.HitCount, CacheHit: e.CacheHit, LatencyMS: e.LatencyMS,
			BackendTimings: e.BackendTimings,
			ExperimentName: e.ExperimentName, ExperimentArm: e.ExperimentArm,
			Source: e.Source,
			Slow:   e.Slow,
		})
	})
	if qaHandler, qerr := admin.NewQueryAnalyticsHandler(queryAnalyticsStore); qerr == nil {
		qaHandler.Register(api)
	}
	// Round-13 Task 8: slow-query admin endpoint.
	admin.NewSlowQueryHandler(queryAnalyticsStore).Register(api)
	// Round-13 Task 9: per-tenant cache-stats admin endpoint.
	admin.NewCacheStatsHandler(queryAnalyticsStore).Register(api)
	// Round-13 Task 10: API key rotation endpoint.
	apiKeyStore := admin.NewAPIKeyStoreGORM(db)
	if err := apiKeyStore.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("api_keys automigrate: %w", err)
	}
	if rotHandler, rerr := admin.NewAPIKeyRotationHandler(admin.APIKeyRotationHandlerConfig{
		Store: apiKeyStore, Audit: notifyingAudit,
	}); rerr == nil {
		rotHandler.Register(api)
	}

	// Round-7 Task 7 / Round-8 Task 11: credential health worker +
	// endpoint. The GORM-backed store reads the credential_* columns
	// migrations/026_credential_valid.sql adds to source_health, so
	// the periodic worker in cmd/ingest and the read endpoint here
	// share one row per source.
	credHealth, err := admin.NewCredentialHealthGORM(db)
	if err != nil {
		return fmt.Errorf("credential_health: %w", err)
	}
	if chHandler, cerr := admin.NewCredentialHealthHandler(credHealth); cerr == nil {
		chHandler.Register(api)
	}

	// Round-7 Task 9: retrieval cache warming endpoint. The
	// executor is a thin closure around retrievalHandler so the
	// admin package stays free of an import on the retrieval
	// package and the inverse cycle is broken.
	cacheWarmer, _ := retrieval.NewCacheWarmer(retrievalHandler, liveResolver)
	cacheWarmExec := admin.CacheWarmExecutorFunc(func(ctx context.Context, tuples []admin.CacheWarmTuple) admin.CacheWarmSummary {
		rtuples := make([]retrieval.WarmTuple, 0, len(tuples))
		for _, t := range tuples {
			rtuples = append(rtuples, retrieval.WarmTuple{TenantID: t.TenantID, Query: t.Query, TopK: t.TopK, Channels: t.Channels, PrivacyMode: t.PrivacyMode})
		}
		rsum := cacheWarmer.Warm(ctx, rtuples)
		out := admin.CacheWarmSummary{Total: rsum.Total, Succeeded: rsum.Succeeded, Failed: rsum.Failed}
		for _, r := range rsum.Results {
			errMsg := ""
			if r.Err != nil {
				errMsg = r.Err.Error()
			}
			out.Results = append(out.Results, admin.CacheWarmResult{
				Query:   r.Tuple.Query,
				Hits:    r.Hits,
				Latency: r.Latency,
				Error:   errMsg,
			})
		}
		return out
	})
	if cwHandler, werr := admin.NewCacheWarmHandler(admin.CacheWarmHandlerConfig{Warmer: cacheWarmExec, Analytics: queryAnalyticsStore, AutoTopN: 10, Audit: notifyingAudit}); werr == nil {
		cwHandler.Register(api)
	}

	// Round-7 Task 10: bulk source operations.
	if bulkHandler, berr := admin.NewBulkSourceHandler(sourceRepo, notifyingAudit); berr == nil {
		bulkHandler.Register(api)
	}

	// Round-7 Task 11 / Round-8 Task 9: per-tenant latency budget.
	latencyBudgetStore, err := admin.NewLatencyBudgetGORM(db)
	if err != nil {
		return fmt.Errorf("latency_budget: %w", err)
	}
	if err := db.AutoMigrate(&admin.LatencyBudget{}); err != nil {
		return fmt.Errorf("latency_budget migrate: %w", err)
	}
	if lbHandler, lerr := admin.NewLatencyBudgetHandler(latencyBudgetStore, notifyingAudit); lerr == nil {
		lbHandler.Register(api)
	}
	// Round-8 Task 9: the retrieval handler consults the per-tenant
	// latency budget on every request through the LatencyBudgetLookup
	// hook, which mirrors the SetQueryAnalyticsRecorder pattern so the
	// admin → retrieval import stays one-way.
	retrievalHandler.SetLatencyBudgetLookup(func(ctx context.Context, tenantID string) (time.Duration, bool) {
		b, err := latencyBudgetStore.Get(ctx, tenantID)
		if err != nil || b == nil || b.MaxLatencyMS <= 0 {
			return 0, false
		}
		return time.Duration(b.MaxLatencyMS) * time.Millisecond, true
	})

	// Round-7 Task 12 / Round-9 Task 5: GORM-backed chunk quality
	// report store.
	cqStore, cqErr := admin.NewChunkQualityStoreGORM(db)
	if cqErr != nil {
		return fmt.Errorf("chunk_quality store: %w", cqErr)
	}
	if merr := cqStore.AutoMigrate(context.Background()); merr != nil {
		return fmt.Errorf("chunk_quality migrate: %w", merr)
	}
	if cqHandler, cerr := admin.NewChunkQualityHandler(cqStore); cerr == nil {
		cqHandler.Register(api)
	}

	// Round-7 Task 14: audit export.
	if aeHandler, aerr := admin.NewAuditExportHandler(auditRepo, notifyingAudit); aerr == nil {
		aeHandler.Register(api)
	}

	// Round-7 Task 15 / Round-8 Task 10: per-tenant cache TTL.
	cacheTTLStore, err := admin.NewCacheTTLGORM(db)
	if err != nil {
		return fmt.Errorf("cache_ttl: %w", err)
	}
	if err := db.AutoMigrate(&admin.CacheConfig{}); err != nil {
		return fmt.Errorf("cache_config migrate: %w", err)
	}
	if ccHandler, cerr := admin.NewCacheConfigHandler(cacheTTLStore, notifyingAudit); cerr == nil {
		ccHandler.Register(api)
	}
	retrievalHandler.SetCacheTTLLookup(func(ctx context.Context, tenantID string, fallback time.Duration) time.Duration {
		return cacheTTLStore.TTLFor(ctx, tenantID, fallback)
	})
	// Round-10 Task 2: wire the same lookup directly into the
	// semantic cache so any caller (cache warmer, batch handler,
	// future surfaces) picks up the tenant-pinned PEXPIRE without
	// duplicating the call-site lookup.
	if sharedSemanticCache != nil {
		sharedSemanticCache.SetTenantTTLLookup(func(ctx context.Context, tenantID string, fallback time.Duration) time.Duration {
			return cacheTTLStore.TTLFor(ctx, tenantID, fallback)
		})
	}

	// Round-7 Task 16 / Round-8 Task 8: sync history.
	syncHistoryStore, err := admin.NewSyncHistoryGORM(db)
	if err != nil {
		return fmt.Errorf("sync_history: %w", err)
	}
	if err := db.AutoMigrate(&admin.SyncHistoryRow{}); err != nil {
		return fmt.Errorf("sync_history migrate: %w", err)
	}
	if shHandler, serr := admin.NewSyncHistoryHandler(syncHistoryStore); serr == nil {
		shHandler.Register(api)
	}

	// Round-7 Task 17 / Round-8 Task 7: pinned retrieval results.
	pinnedStore, err := admin.NewPinnedResultStoreGORM(db)
	if err != nil {
		return fmt.Errorf("pinned_results: %w", err)
	}
	if err := pinnedStore.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("pinned_results migrate: %w", err)
	}
	if prHandler, perr := admin.NewPinnedResultsHandler(pinnedStore, notifyingAudit); perr == nil {
		prHandler.Register(api)
	}
	// Round-8 Task 16: pin lookup hook so retrieval can call into
	// the GORM-backed store after policy filtering and before MMR.
	retrievalHandler.SetPinLookup(func(ctx context.Context, tenantID, query string) []retrieval.PinnedHit {
		rows, err := pinnedStore.Lookup(ctx, tenantID, query)
		if err != nil || len(rows) == 0 {
			return nil
		}
		out := make([]retrieval.PinnedHit, 0, len(rows))
		for _, r := range rows {
			out = append(out, retrieval.PinnedHit{ChunkID: r.ChunkID, Position: r.Position})
		}
		return out
	})

	// Round-7 Task 18: pipeline health dashboard.
	if phAgg, perr := admin.NewPipelineHealthAggregator(observability.Registry); perr == nil {
		if phHandler, herr := admin.NewPipelineHealthHandler(phAgg); herr == nil {
			phHandler.Register(api)
		}
	}

	// Phase 6 / Tasks 14 + 17: B2C client SDK surface. Mounts
	// /v1/health, /v1/capabilities, and /v1/sync/schedule.
	b2cHandler, err := b2c.NewHandler(b2c.HandlerConfig{
		EnabledBackends: enabledBackends(handlerCfg),
		LocalShardSync:  true,
		DeviceFirst:     handlerCfg.ShardVersionLookup != nil,
	})
	if err != nil {
		return fmt.Errorf("b2c handler: %w", err)
	}
	b2cHandler.Register(api)

	// Phase 1 Task 19 retrieval optimisations: pre-warm Qdrant
	// connections so the first user-facing /v1/retrieve doesn't pay
	// the connection-establishment tax, and start a FalkorDB
	// keep-alive ticker so the redis pool stays warm.
	warmCtx, warmCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := qdrant.Warmup(warmCtx, qdrantPool/4+1); err != nil {
		slog.Warn("api: qdrant warmup", slog.String("error", err.Error()))
	}
	warmCancel()
	keepAliveCtx, keepAliveCancel := context.WithCancel(context.Background())
	defer keepAliveCancel()
	if handlerCfg.Graph != nil && sharedRedis != nil {
		if falkor, ferr := storage.NewFalkorDBClient(storage.FalkorDBConfig{
			Client: storage.FalkorDBFromRedis(sharedRedis),
		}); ferr == nil {
			go falkor.KeepAlive(keepAliveCtx, 30*time.Second)
		}
	}

	// Round-9 Task 17: periodic connection-pool health sampler.
	// Publishes context_engine_postgres_pool_open_connections,
	// context_engine_redis_pool_active_connections, and
	// context_engine_qdrant_pool_idle_connections every 30s so
	// operators can dashboard / alert on pool exhaustion.
	poolSampler := observability.NewPoolSampler(observability.PoolSamplerConfig{
		Postgres: observability.PostgresStatsFunc(func() int {
			sqlDB, derr := db.DB()
			if derr != nil || sqlDB == nil {
				return 0
			}
			return sqlDB.Stats().OpenConnections
		}),
		Redis: observability.RedisStatsFunc(func() int {
			if sharedRedis == nil {
				return 0
			}
			stats := sharedRedis.PoolStats()
			if stats == nil {
				return 0
			}
			active := int(stats.TotalConns) - int(stats.IdleConns)
			if active < 0 {
				return 0
			}
			return active
		}),
		Qdrant: observability.QdrantStatsFunc(func() int {
			if qdrant == nil {
				return 0
			}
			return qdrant.IdleConnCapacity()
		}),
		Interval: 30 * time.Second,
	})
	go poolSampler.Start(keepAliveCtx)

	// Round-13 Task 18: Postgres pool leak detector. Sustained
	// utilisation above 90% across three consecutive samples
	// produces a structured warning; the percent reading is also
	// published to the context_engine_postgres_pool_utilization_percent
	// gauge for dashboards.
	if pgMax, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_PG_MAX_OPEN")); pgMax > 0 {
		ldet, lerr := observability.NewPoolLeakDetector(observability.PoolLeakDetectorConfig{
			Pool: observability.PostgresStatsFunc(func() int {
				sqlDB, derr := db.DB()
				if derr != nil || sqlDB == nil {
					return 0
				}
				return sqlDB.Stats().OpenConnections
			}),
			MaxOpen:  pgMax,
			Interval: 30 * time.Second,
		})
		if lerr == nil {
			go ldet.Start(keepAliveCtx)
		}
	}

	// Round-10 Task 6: background OAuth token refresh worker.
	// Scans every active source on `CONTEXT_ENGINE_TOKEN_REFRESH_INTERVAL`
	// (default 15m) and refreshes any credential whose expiry is
	// inside admin.RefreshWindow. Off when no admin DB is wired or
	// when the env var is set to "0" / "off". Errors per-source are
	// logged via the worker's own observability path.
	tokenRefreshDone := make(chan struct{})
	tokenRefreshCtx, tokenRefreshCancel := context.WithCancel(context.Background())
	defer tokenRefreshCancel()
	if v := strings.TrimSpace(os.Getenv("CONTEXT_ENGINE_TOKEN_REFRESH_INTERVAL")); v != "0" && v != "off" {
		interval := 15 * time.Minute
		if v != "" {
			if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
				interval = d
			}
		}
		tokenWorker, twErr := admin.NewTokenRefreshWorker(admin.TokenRefreshWorkerConfig{
			Lister:    sourceRepo,
			Updater:   sourceRepo,
			Refresher: admin.NewOAuth2RefreshClient(nil),
			Audit:     notifyingAudit,
			Interval:  interval,
		})
		if twErr != nil {
			slog.Warn("api: token-refresh worker disabled", slog.String("error", twErr.Error()))
			close(tokenRefreshDone)
		} else {
			go func() {
				defer close(tokenRefreshDone)
				if err := tokenWorker.Run(tokenRefreshCtx); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("api: token-refresh worker exited", slog.String("error", err.Error()))
				}
			}()
			slog.Info("api: token-refresh worker started", slog.Duration("interval", interval))
		}
	} else {
		close(tokenRefreshDone)
	}

	srv := &http.Server{Addr: listenAddr, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("api: listening", slog.String("addr", listenAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-stop:
		slog.Info("api: shutting down")
	}

	timeout := 15 * time.Second
	if v, _ := strconv.Atoi(os.Getenv("CONTEXT_ENGINE_SHUTDOWN_TIMEOUT_SECONDS")); v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	sd := lifecycle.New(timeout, slog.Default())
	sd.Add("http-server", func(ctx context.Context) error { return srv.Shutdown(ctx) })
	// Round-10 Task 6: token-refresh worker shutdown — cancel its
	// context and wait for the goroutine to drain so its in-flight
	// HTTP exchange to the OAuth issuer completes before redis /
	// postgres connections drop.
	sd.Add("token-refresh-worker", func(ctx context.Context) error {
		tokenRefreshCancel()
		select {
		case <-tokenRefreshDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if sharedRedis != nil {
		sd.Add("redis", func(_ context.Context) error { return sharedRedis.Close() })
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sd.Add("postgres", func(_ context.Context) error { return sqlDB.Close() })
	}
	return sd.Run(context.Background())
}

// authPlaceholder is the Phase 1 placeholder for OIDC middleware. It
// reads the tenant from X-Tenant-ID and the actor from X-Actor-ID,
// rejecting requests that lack a tenant. The real auth tier (BFF or
// OIDC) will replace this in Phase 2.
func authPlaceholder(c *gin.Context) {
	tenantID := c.GetHeader("X-Tenant-ID")
	if tenantID == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing X-Tenant-ID"})

		return
	}
	c.Set(audit.TenantContextKey, tenantID)
	if actorID := c.GetHeader("X-Actor-ID"); actorID != "" {
		c.Set(audit.ActorContextKey, actorID)
	}
	c.Next()
}

// enabledBackends inspects the assembled retrieval HandlerConfig
// and returns the names of the fan-out backends that are wired (i.e.
// have a non-nil client). The B2C capabilities endpoint surfaces
// this list so clients can grey out UI affordances for backends the
// server isn't configured to use.
func enabledBackends(cfg retrieval.HandlerConfig) []string {
	out := []string{}
	if cfg.VectorStore != nil {
		out = append(out, "vector")
	}
	if cfg.BM25 != nil {
		out = append(out, "bm25")
	}
	if cfg.Graph != nil {
		out = append(out, "graph")
	}
	if cfg.Memory != nil {
		out = append(out, "memory")
	}
	return out
}

// queryEmbedAdapter satisfies retrieval.Embedder by calling
// pipeline.Embedder.EmbedTexts with a single query string.
type queryEmbedAdapter struct {
	e *pipeline.Embedder
}

func (a *queryEmbedAdapter) EmbedQuery(ctx context.Context, tenantID, query string) ([]float32, error) {
	out, _, err := a.e.EmbedTexts(ctx, tenantID, []string{query})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("embed: no embedding returned")
	}

	return out[0], nil
}

// simulatorRetriever bridges the policy.Simulator's Retriever port
// to retrieval.Handler.RetrieveWithSnapshot. The simulator passes a
// SimRetrieveRequest + draft PolicySnapshot; we project that into a
// retrieval.RetrieveRequest, run the full pipeline against the
// supplied snapshot (skipping the cache), then re-project the
// resulting retrieval.RetrieveHits back into the policy-package
// RetrieveHit shape so the simulator stays free of retrieval-
// transitive deps.
func simulatorRetriever(h *retrieval.Handler) policy.RetrieverFunc {
	return func(ctx context.Context, req policy.SimRetrieveRequest, snap policy.PolicySnapshot) ([]policy.RetrieveHit, error) {
		channels := []string{}
		if req.ChannelID != "" {
			channels = append(channels, req.ChannelID)
		}
		retrievalReq := retrieval.RetrieveRequest{
			Query:    req.Query,
			TopK:     req.TopK,
			Channels: channels,
			SkillID:  req.SkillID,
		}
		resp, err := h.RetrieveWithSnapshot(ctx, req.TenantID, retrievalReq, snap)
		if err != nil {
			return nil, err
		}
		out := make([]policy.RetrieveHit, 0, len(resp.Hits))
		for _, hit := range resp.Hits {
			out = append(out, policy.RetrieveHit{
				ID:           hit.ID,
				Score:        hit.Score,
				TenantID:     hit.TenantID,
				SourceID:     hit.SourceID,
				DocumentID:   hit.DocumentID,
				BlockID:      hit.BlockID,
				Title:        hit.Title,
				URI:          hit.URI,
				PrivacyLabel: hit.PrivacyLabel,
				Connector:    hit.Connector,
				Sources:      hit.Sources,
				Metadata:     hit.Metadata,
			})
		}
		return out, nil
	}
}

// evalRetriever bridges the eval runner's neutral RetrieveFunc to
// retrieval.Handler.RetrieveWithSnapshotCached. We deliberately
// re-use the live path (resolver + cache + fan-out + merge + ACL
// gate) so the eval harness scores the system the same way
// production traffic experiences it. When the resolver fails the
// closure returns the error so the runner records a per-case
// failure rather than silently masking the regression.
func evalRetriever(h *retrieval.Handler, resolver policy.PolicyResolver) eval.RetrieveFunc {
	return func(ctx context.Context, req eval.RetrieveRequest) ([]eval.RetrieveHit, error) {
		snap, err := resolver.Resolve(ctx, req.TenantID, "")
		if err != nil {
			return nil, err
		}
		resp, err := h.RetrieveWithSnapshotCached(ctx, req.TenantID, retrieval.RetrieveRequest{
			Query:       req.Query,
			TopK:        req.TopK,
			SkillID:     req.SkillID,
			PrivacyMode: req.PrivacyMode,
		}, snap)
		if err != nil {
			return nil, err
		}
		out := make([]eval.RetrieveHit, 0, len(resp.Hits))
		for _, hit := range resp.Hits {
			out = append(out, eval.RetrieveHit{ID: hit.ID, Score: hit.Score})
		}
		return out, nil
	}
}

// memoryAdapter wraps the Mem0 gRPC client into the
// retrieval.MemoryBackend interface, projecting SearchMemoryResult
// records into retrieval.Match.
type memoryAdapter struct {
	c memoryv1.MemoryServiceClient
}

func (m *memoryAdapter) Search(ctx context.Context, tenantID, query string, k int) ([]*retrieval.Match, error) {
	resp, err := m.c.SearchMemory(ctx, &memoryv1.SearchMemoryRequest{
		TenantId: tenantID,
		Query:    query,
		TopK:     int32(k), //nolint:gosec // k is bounded by handler MaxTopK
	})
	if err != nil {
		return nil, err
	}
	out := make([]*retrieval.Match, 0, len(resp.GetResults()))
	for i, r := range resp.GetResults() {
		rec := r.GetRecord()
		if rec == nil {
			continue
		}
		out = append(out, &retrieval.Match{
			ID:       rec.GetId(),
			Source:   retrieval.SourceMemory,
			Score:    r.GetScore(),
			Rank:     i + 1,
			TenantID: rec.GetTenantId(),
			Text:     rec.GetContent(),
		})
	}

	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

// isAutoMigrateEnabled returns true when either AUTO_MIGRATE or
// CONTEXT_ENGINE_AUTO_MIGRATE is set to a truthy value.
func isAutoMigrateEnabled() bool {
	for _, k := range []string{"AUTO_MIGRATE", "CONTEXT_ENGINE_AUTO_MIGRATE"} {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
		switch v {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func parseInt(s string) (int, error) {
	var n int
	for i, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not an integer at index %d: %q", i, s)
		}
		n = n*10 + int(ch-'0')
	}
	if s == "" {
		return 0, errors.New("empty integer")
	}

	return n, nil
}
