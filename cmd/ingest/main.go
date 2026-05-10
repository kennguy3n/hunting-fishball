// Command context-engine-ingest is the Kafka consumer that drives
// the 4-stage ingestion pipeline.
//
// Phase 1 wiring:
//   - Connects to Kafka (sarama ConsumerGroup) for the ingest /
//     reindex / purge topics.
//   - Connects to PostgreSQL (chunk metadata) and Qdrant (vectors).
//   - Connects to the Docling parsing gRPC service and the embedding
//     gRPC service.
//   - Builds the 4-stage pipeline (fetch → parse → embed → store) and
//     submits Kafka messages onto it.
//   - Routes failed messages to the configured DLQ topic.
//   - Graceful shutdown on SIGINT / SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	// Blank-imports register each connector in the global registry via
	// init(). Order is alphabetical to keep diffs minimal.
	"github.com/kennguy3n/hunting-fishball/internal/config"
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
	"github.com/kennguy3n/hunting-fishball/internal/lifecycle"
	"github.com/kennguy3n/hunting-fishball/internal/migrate"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
	doclingv1 "github.com/kennguy3n/hunting-fishball/proto/docling/v1"
	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
)

func main() {
	// Phase 8 Task 17: structured JSON logging. Same shape as the API
	// binary so log shippers don't need a per-component schema. The
	// component label is embedded once on the logger; downstream
	// callers add tenant_id / trace_id / event-specific fields via
	// LoggerFromContext + With.
	logger := observability.NewLogger("ingest")
	observability.SetDefault(logger)
	slog.SetDefault(logger)
	if err := run(); err != nil {
		logger.Error("ingest: fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	if err := config.ValidateIngest(config.OSLooker); err != nil {
		return err
	}
	dsn := envOr("CONTEXT_ENGINE_DATABASE_URL", "")
	brokers := envOr("CONTEXT_ENGINE_KAFKA_BROKERS", "localhost:9092")
	groupID := envOr("CONTEXT_ENGINE_KAFKA_GROUP", "context-engine-ingest")
	topics := envOr("CONTEXT_ENGINE_KAFKA_TOPICS", "ingest,reindex,purge")
	dlqTopic := envOr("CONTEXT_ENGINE_KAFKA_DLQ_TOPIC", "ingest.dlq")
	qdrantURL := envOr("CONTEXT_ENGINE_QDRANT_URL", "http://localhost:6333")
	parseTarget := envOr("CONTEXT_ENGINE_PARSE_TARGET", "localhost:8082")
	embedTarget := envOr("CONTEXT_ENGINE_EMBEDDING_TARGET", "localhost:8081")
	vectorSizeStr := envOr("CONTEXT_ENGINE_VECTOR_SIZE", "1536")
	vectorSize, err := parseInt(vectorSizeStr)
	if err != nil {
		return fmt.Errorf("CONTEXT_ENGINE_VECTOR_SIZE: %w", err)
	}

	slog.Info("ingest: registered connectors", slog.Any("connectors", connector.ListSourceConnectors()))

	// ---- DB
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	// Phase 8 / Task 18: optional automatic SQL migration runner.
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
		slog.Info("ingest: applied migrations", slog.Int("count", len(applied)))
	}
	pgStore, err := storage.NewPostgresStore(db)
	if err != nil {
		return fmt.Errorf("postgres store: %w", err)
	}
	if err := pgStore.AutoMigrate(context.Background()); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// ---- Vector store
	qdrant, err := storage.NewQdrantClient(storage.QdrantConfig{
		BaseURL:    qdrantURL,
		VectorSize: vectorSize,
	})
	if err != nil {
		return fmt.Errorf("qdrant: %w", err)
	}

	// ---- Parsing gRPC
	parseConn, err := grpc.NewClient(parseTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial parse: %w", err)
	}
	defer func() { _ = parseConn.Close() }()
	parser, err := pipeline.NewParser(pipeline.ParseConfig{
		Client: doclingv1.NewDoclingServiceClient(parseConn),
	})
	if err != nil {
		return fmt.Errorf("parser: %w", err)
	}

	// ---- Embedding gRPC
	embedConn, err := grpc.NewClient(embedTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial embed: %w", err)
	}
	defer func() { _ = embedConn.Close() }()
	embedder, err := pipeline.NewEmbedder(pipeline.EmbedConfig{
		Local: embeddingv1.NewEmbeddingServiceClient(embedConn),
	})
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}

	// ---- Stage 4 storer
	fetcher := pipeline.NewFetcher(pipeline.FetchConfig{})
	storer, err := pipeline.NewStorer(pipeline.StoreConfig{
		Vector:    qdrant,
		Metadata:  pgStore,
		Connector: "kafka",
	})
	if err != nil {
		return fmt.Errorf("storer: %w", err)
	}

	// ---- Coordinator
	// Phase 8 per-stage worker pool sizing. Each stage is a CPU/IO
	// trade-off in its own right (Fetch dominates on connector
	// network throughput; Embed wants GPU concurrency; Store wants
	// pgx pool headroom). The defaults stay 1 per stage so existing
	// deployments preserve their current single-threaded behaviour.
	stageWorkers := pipeline.StageConfig{
		FetchWorkers: stageWorkerEnv("CONTEXT_ENGINE_FETCH_WORKERS"),
		ParseWorkers: stageWorkerEnv("CONTEXT_ENGINE_PARSE_WORKERS"),
		EmbedWorkers: stageWorkerEnv("CONTEXT_ENGINE_EMBED_WORKERS"),
		StoreWorkers: stageWorkerEnv("CONTEXT_ENGINE_STORE_WORKERS"),
	}
	coord, err := pipeline.NewCoordinator(pipeline.CoordinatorConfig{
		Fetch:   fetcher,
		Parse:   parser,
		Embed:   embedder,
		Store:   storer,
		Workers: stageWorkers,
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}

	// ---- Kafka group + DLQ producer
	saramaCfg := pipeline.SaramaConfig()
	brokerList := strings.Split(brokers, ",")
	group, err := sarama.NewConsumerGroup(brokerList, groupID, saramaCfg)
	if err != nil {
		return fmt.Errorf("kafka group: %w", err)
	}
	defer func() { _ = group.Close() }()

	dlqProducer, err := sarama.NewSyncProducer(brokerList, saramaCfg)
	if err != nil {
		return fmt.Errorf("kafka dlq producer: %w", err)
	}
	defer func() { _ = dlqProducer.Close() }()

	cons, err := pipeline.NewConsumer(group, pipeline.ConsumerConfig{
		Brokers:     brokerList,
		GroupID:     groupID,
		Topics:      strings.Split(topics, ","),
		DLQTopic:    dlqTopic,
		DLQProducer: dlqProducer,
		Coordinator: coord,
	})
	if err != nil {
		return fmt.Errorf("consumer: %w", err)
	}

	// ---- Run + signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// coordDone / consDone are closed (in addition to receiving the
	// final error) so a second receive after the channel has been
	// drained returns the zero value immediately rather than blocking.
	// The graceful-shutdown step for the pipeline coordinator relies
	// on this — see waitChanClosed and its regression test.
	coordDone := make(chan error, 1)
	go func() {
		coordDone <- coord.Run(ctx)
		close(coordDone)
	}()

	consDone := make(chan error, 1)
	go func() {
		consDone <- cons.Run(ctx)
		close(consDone)
	}()

	// Phase 8 / Task 15: optional DLQ observer. Enables a sidecar
	// consumer that watches the dead-letter topic and emits both a
	// structured slog line and the `context_engine_dlq_messages_total`
	// counter for every dead-letter message. Off by default so single-
	// binary deployments don't sprout a second consumer group.
	if os.Getenv("CONTEXT_ENGINE_DLQ_OBSERVE") == "1" {
		dlqGroupID := envOr("CONTEXT_ENGINE_DLQ_OBSERVER_GROUP", groupID+"-dlq-observer")
		dlqGroup, gerr := sarama.NewConsumerGroup(brokerList, dlqGroupID, saramaCfg)
		if gerr != nil {
			return fmt.Errorf("kafka dlq observer group: %w", gerr)
		}
		defer func() { _ = dlqGroup.Close() }()
		dlqObs, oerr := pipeline.NewDLQObserver(pipeline.DLQObserverConfig{
			Group:  dlqGroup,
			Topic:  dlqTopic,
			Logger: slog.Default(),
		})
		if oerr != nil {
			return fmt.Errorf("dlq observer: %w", oerr)
		}
		go func() {
			if err := dlqObs.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("ingest: dlq observer", slog.String("error", err.Error()))
			}
		}()
	}

	// Task 6: optional retention worker. Sweeps the chunks table per
	// tenant and evicts rows whose ingested_at exceeds the effective
	// MaxAgeDays of the resolved retention policy. Off by default; set
	// CONTEXT_ENGINE_RETENTION_INTERVAL=1h (any duration string) to
	// enable.
	if v := os.Getenv("CONTEXT_ENGINE_RETENTION_INTERVAL"); v != "" {
		interval, perr := time.ParseDuration(v)
		if perr != nil {
			return fmt.Errorf("retention interval: %w", perr)
		}
		policySrc := pipeline.NewRetentionPolicySourceGORM(db)
		if err := policySrc.AutoMigrate(ctx); err != nil {
			return fmt.Errorf("retention policy migrate: %w", err)
		}
		chunkSrc := pipeline.NewRetentionChunkSourceGORM(db)
		deleter := pipeline.NewComboRetentionDeleter(pgStore, qdrant)
		retWorker, werr := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
			Chunks: chunkSrc, Policies: policySrc, Deleter: deleter,
			Logger: slog.Default(), Interval: interval,
		})
		if werr != nil {
			return fmt.Errorf("retention worker: %w", werr)
		}
		go func() {
			if err := retWorker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("ingest: retention worker", slog.String("error", err.Error()))
			}
		}()
	}

	// Task 5: optional persistent DLQ consumer. Lands every dead-letter
	// envelope in Postgres so operators can list / replay failed events
	// from /v1/admin/dlq. Off by default so legacy deployments don't
	// gain a new consumer group accidentally.
	if os.Getenv("CONTEXT_ENGINE_DLQ_CONSUME") == "1" {
		dlqStore := pipeline.NewDLQStoreGORM(db)
		if err := dlqStore.AutoMigrate(ctx); err != nil {
			return fmt.Errorf("dlq store automigrate: %w", err)
		}
		dlqGroupID := envOr("CONTEXT_ENGINE_DLQ_CONSUMER_GROUP", groupID+"-dlq-consumer")
		dlqGroup, gerr := sarama.NewConsumerGroup(brokerList, dlqGroupID, saramaCfg)
		if gerr != nil {
			return fmt.Errorf("kafka dlq consumer group: %w", gerr)
		}
		defer func() { _ = dlqGroup.Close() }()
		dlqCons, cerr := pipeline.NewDLQConsumer(pipeline.DLQConsumerConfig{
			Group:       dlqGroup,
			Topic:       dlqTopic,
			Store:       dlqStore,
			Logger:      slog.Default(),
			MaxAttempts: stageWorkerEnv("CONTEXT_ENGINE_DLQ_MAX_ATTEMPTS"),
		})
		if cerr != nil {
			return fmt.Errorf("dlq consumer: %w", cerr)
		}
		go func() {
			if err := dlqCons.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("ingest: dlq consumer", slog.String("error", err.Error()))
			}
		}()
	}

	// ---- Phase 8 Task 20: HTTP probes + /metrics on a sidecar port.
	var redisClient *redis.Client
	if u := os.Getenv("CONTEXT_ENGINE_REDIS_URL"); u != "" {
		if opts, perr := redis.ParseURL(u); perr == nil {
			redisClient = redis.NewClient(opts)
		}
	}
	httpAddr := envOr("CONTEXT_ENGINE_INGEST_HTTP_ADDR", ":8090")
	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           ingestHTTPHandler(db, redisClient, brokerList),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("ingest: probes/metrics listening", slog.String("addr", httpAddr))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("ingest: http server", slog.String("error", err.Error()))
		}
	}()

	slog.Info("ingest: started", slog.String("topics", topics), slog.String("group", groupID))
	select {
	case sig := <-stop:
		slog.Info("ingest: shutting down", slog.String("signal", sig.String()))
	case err := <-consDone:
		if err != nil {
			return fmt.Errorf("consumer exited: %w", err)
		}
	case err := <-coordDone:
		if err != nil {
			return fmt.Errorf("coordinator exited: %w", err)
		}
	}
	cancel()
	timeout := 30 * time.Second
	if v, _ := parseInt(os.Getenv("CONTEXT_ENGINE_SHUTDOWN_TIMEOUT_SECONDS")); v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	sd := lifecycle.New(timeout, slog.Default())
	sd.Add("kafka-consumer", func(_ context.Context) error { return cons.Stop() })
	sd.Add("pipeline-coordinator", func(ctx context.Context) error {
		coord.CloseInputs()
		return waitChanClosed(ctx, coordDone)
	})
	sd.Add("http-server", func(ctx context.Context) error { return httpSrv.Shutdown(ctx) })
	if sqlDB, derr := db.DB(); derr == nil {
		sd.Add("postgres", func(_ context.Context) error { return sqlDB.Close() })
	}
	if redisClient != nil {
		sd.Add("redis", func(_ context.Context) error { return redisClient.Close() })
	}
	return sd.Run(context.Background())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

// waitChanClosed blocks until ch delivers a value, ch is closed, or ctx
// expires. The original Phase 8 / Task 10 lifecycle step did the same
// receive inline, but if the value had already been consumed by the
// outer select (e.g. the coordinator exited cleanly before SIGTERM and
// the main loop took its err==nil case), the inline `<-ch` blocked for
// the full lifecycle deadline before returning ctx.Err(), starving
// every subsequent shutdown step. The fix has two halves:
//   - the producer goroutines close ch after sending, so a second
//     receive returns the zero value immediately;
//   - this helper makes that contract explicit and gives the
//     regression test a stable surface to exercise.
func waitChanClosed(ctx context.Context, ch <-chan error) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isAutoMigrateEnabled mirrors cmd/api: AUTO_MIGRATE / CONTEXT_ENGINE_AUTO_MIGRATE
// = 1/true/yes/on enables the SQL migration runner on startup.
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

// stageWorkerEnv reads an integer worker count from the named env
// var. Empty / non-numeric / non-positive values fall back to 0 so
// pipeline.StageConfig.defaults() applies the per-stage default of
// 1, preserving the pre-Phase-8 single-goroutine behaviour for any
// stage the operator hasn't explicitly opted in to fan out.
func stageWorkerEnv(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := parseInt(v)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

func parseInt(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty integer")
	}
	var n int
	for i, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not an integer at index %d: %q", i, s)
		}
		n = n*10 + int(ch-'0')
	}

	return n, nil
}
