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
	dsn := envOr("CONTEXT_ENGINE_DATABASE_URL", "")
	if dsn == "" {
		return errors.New("CONTEXT_ENGINE_DATABASE_URL is required")
	}
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

	coordDone := make(chan error, 1)
	go func() { coordDone <- coord.Run(ctx) }()

	consDone := make(chan error, 1)
	go func() { consDone <- cons.Run(ctx) }()

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
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpSrv.Shutdown(shutdownCtx)
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
	_ = cons.Stop()
	coord.CloseInputs()
	// Drain coordDone non-blockingly: if the select above already fired
	// the coordDone case, the value has been consumed and the goroutine
	// has exited, so a blocking <-coordDone would deadlock here.
	select {
	case <-coordDone:
	default:
	}

	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
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
