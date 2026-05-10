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
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/box"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/github"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/jira"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/notion"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
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
		handlerCfg.Cache, err = storage.NewSemanticCache(storage.SemanticCacheConfig{
			Client:    rc,
			KeyPrefix: "hf:",
		})
		if err != nil {
			return fmt.Errorf("semantic cache: %w", err)
		}
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
	r.GET("/metrics", gin.WrapH(observability.Handler()))
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/readyz", apiReadyzHandler(db, sharedRedis, qdrant))

	apiMiddlewares := []gin.HandlerFunc{authPlaceholder, observability.GinLoggerMiddleware("api")}

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
	retrievalHandler.Register(api)

	// Admin source-management surface (Phase 2). The handler mounts
	// POST/GET/PATCH/DELETE under /v1/admin/sources and tolerates a
	// nil PolicyResolver / EventEmitter for the bare-bones config —
	// production wiring fills those in.
	sourceRepo := admin.NewSourceRepository(db)
	healthRepo := admin.NewHealthRepository(db, admin.DefaultThresholds)
	adminHandler, err := admin.NewHandler(admin.HandlerConfig{
		Repo:   sourceRepo,
		Audit:  auditRepo,
		Health: healthRepo,
	})
	if err != nil {
		return fmt.Errorf("admin handler: %w", err)
	}
	adminHandler.Register(api)

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
		reindexHandler, err := admin.NewReindexHandler(admin.ReindexHandlerConfig{Runner: orch, Audit: auditRepo})
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
		Audit:  auditRepo,
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
		Audit:     auditRepo,
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
		Audit:     auditRepo,
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
