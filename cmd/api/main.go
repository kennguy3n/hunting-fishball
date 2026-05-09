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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
	memoryv1 "github.com/kennguy3n/hunting-fishball/proto/memory/v1"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("api: %v", err)
	}
}

func run() error {
	listenAddr := envOr("CONTEXT_ENGINE_LISTEN_ADDR", ":8080")
	dsn := envOr("CONTEXT_ENGINE_DATABASE_URL", "")
	qdrantURL := envOr("CONTEXT_ENGINE_QDRANT_URL", "http://localhost:6333")
	embedTarget := envOr("CONTEXT_ENGINE_EMBEDDING_TARGET", "localhost:8081")
	vectorSizeStr := envOr("CONTEXT_ENGINE_VECTOR_SIZE", "1536")
	vectorSize, err := parseInt(vectorSizeStr)
	if err != nil {
		return fmt.Errorf("CONTEXT_ENGINE_VECTOR_SIZE: %w", err)
	}

	if dsn == "" {
		return errors.New("CONTEXT_ENGINE_DATABASE_URL is required")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}

	auditRepo := audit.NewRepository(db)

	qdrant, err := storage.NewQdrantClient(storage.QdrantConfig{
		BaseURL:    qdrantURL,
		VectorSize: vectorSize,
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

	handlerCfg := retrieval.HandlerConfig{
		VectorStore: qdrant,
		Embedder:    queryEmbedder,
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
	if redisURL := os.Getenv("CONTEXT_ENGINE_REDIS_URL"); redisURL != "" {
		opts, perr := redis.ParseURL(redisURL)
		if perr != nil {
			return fmt.Errorf("redis url: %w", perr)
		}
		rc := redis.NewClient(opts)
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
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	api := r.Group("/", authPlaceholder)
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

	srv := &http.Server{Addr: listenAddr, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("api: listening on %s", listenAddr)
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
		log.Println("api: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
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
