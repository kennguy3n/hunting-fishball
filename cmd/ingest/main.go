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
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/IBM/sarama"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
	doclingv1 "github.com/kennguy3n/hunting-fishball/proto/docling/v1"
	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("ingest: %v", err)
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

	log.Printf("ingest: registered connectors: %v", connector.ListSourceConnectors())

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
	coord, err := pipeline.NewCoordinator(pipeline.CoordinatorConfig{
		Fetch: fetcher,
		Parse: parser,
		Embed: embedder,
		Store: storer,
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

	log.Printf("ingest: started; topics=%s group=%s", topics, groupID)
	select {
	case sig := <-stop:
		log.Printf("ingest: shutting down on signal %s", sig)
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
