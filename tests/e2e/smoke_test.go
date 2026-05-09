//go:build e2e

// Package e2e runs the Phase 1 smoke test against a live storage plane
// (the docker-compose stack: Postgres + Qdrant + Kafka + Redis).
//
// The test does not start the api / ingest binaries — that's left to
// the CI workflow which runs `go test -tags=e2e ./tests/e2e/...` after
// `docker compose up -d`. The test exercises the same code paths the
// binaries use:
//
//  1. Build a 4-stage pipeline with mock Fetch / Parse / Embed stages
//     and the real Postgres + Qdrant Stage 4 storer. Submit a sample
//     "document" through the coordinator and assert it lands in Qdrant
//     and Postgres.
//  2. Wire up the retrieval handler and submit POST /v1/retrieve over
//     httptest.NewRecorder, confirming the previously-stored vector
//     comes back as the top hit.
//  3. Insert a synthetic audit-log entry to verify the audit
//     repository writes through to the same Postgres database.
//
// To run: `make test-e2e`.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// fakeFetch returns the inline content directly.
type fakeFetch struct{}

func (fakeFetch) FetchEvent(_ context.Context, evt pipeline.IngestEvent) (*pipeline.Document, error) {
	return &pipeline.Document{
		TenantID:    evt.TenantID,
		SourceID:    evt.SourceID,
		DocumentID:  evt.DocumentID,
		NamespaceID: evt.NamespaceID,
		Title:       evt.Title,
		MIMEType:    evt.MIMEType,
		Content:     evt.InlineContent,
		ContentHash: pipeline.HashContent(evt.InlineContent),
		IngestedAt:  time.Now().UTC(),
		Metadata:    evt.Metadata,
	}, nil
}

// fakeParse returns one block per kilobyte of content.
type fakeParse struct{}

func (fakeParse) Parse(_ context.Context, doc *pipeline.Document) ([]pipeline.Block, error) {
	return []pipeline.Block{
		{BlockID: doc.DocumentID + "-b0", Text: string(doc.Content), Type: "paragraph", PrivacyLabel: doc.PrivacyLabel},
	}, nil
}

// fakeEmbed returns deterministic 8-dim vectors derived from the block index.
type fakeEmbed struct{}

func (fakeEmbed) EmbedBlocks(_ context.Context, _ string, blocks []pipeline.Block) ([][]float32, string, error) {
	out := make([][]float32, len(blocks))
	for i := range blocks {
		v := make([]float32, 8)
		for j := range v {
			v[j] = float32(j+1) / 10.0
		}
		out[i] = v
	}

	return out, "fake-model", nil
}

func (fakeEmbed) EmbedQuery(_ context.Context, _ string, _ string) ([]float32, error) {
	v := make([]float32, 8)
	for j := range v {
		v[j] = float32(j+1) / 10.0
	}

	return v, nil
}

// envOr returns the env var or def when not set.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

// requireE2E skips the test unless E2E_ENABLED=1 is set.
func requireE2E(t *testing.T) {
	if os.Getenv("E2E_ENABLED") != "1" {
		t.Skip("E2E_ENABLED!=1; skipping e2e smoke test")
	}
}

func TestSmoke_PipelineAndRetrieval(t *testing.T) {
	requireE2E(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsn := envOr("CONTEXT_ENGINE_DATABASE_URL", "host=localhost user=hf password=hf dbname=hunting_fishball port=5432 sslmode=disable")
	qdrantURL := envOr("CONTEXT_ENGINE_QDRANT_URL", "http://localhost:6333")

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}

	pgStore, err := storage.NewPostgresStore(db)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	if err := pgStore.AutoMigrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	qdrant, err := storage.NewQdrantClient(storage.QdrantConfig{
		BaseURL:    qdrantURL,
		VectorSize: 8,
	})
	if err != nil {
		t.Fatalf("qdrant: %v", err)
	}

	storer, err := pipeline.NewStorer(pipeline.StoreConfig{
		Vector:    qdrant,
		Metadata:  pgStore,
		Connector: "smoke-test",
	})
	if err != nil {
		t.Fatalf("storer: %v", err)
	}

	coord, err := pipeline.NewCoordinator(pipeline.CoordinatorConfig{
		Fetch:          fakeFetch{},
		Parse:          fakeParse{},
		Embed:          fakeEmbed{},
		Store:          storer,
		QueueSize:      4,
		MaxAttempts:    1,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	tenantID := fmt.Sprintf("smoke-%d", time.Now().UnixNano())
	docID := "doc-smoke-1"
	body := []byte("hello e2e smoke test world")

	runDone := make(chan error, 1)
	go func() { runDone <- coord.Run(ctx) }()

	if err := coord.Submit(ctx, pipeline.IngestEvent{
		Kind:          pipeline.EventDocumentChanged,
		TenantID:      tenantID,
		SourceID:      "smoke",
		DocumentID:    docID,
		NamespaceID:   "ns",
		Title:         "smoke-doc",
		MIMEType:      "text/plain",
		PrivacyLabel:  "internal",
		InlineContent: body,
		Metadata:      map[string]string{"uri": "smoke://doc-1"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	coord.CloseInputs()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Insert an audit-log entry to verify Postgres write-through.
	auditRepo := audit.NewRepository(db)
	row := audit.NewAuditLog(tenantID, "00000000000000000000000000", audit.ActionSourceSynced, "document", docID, nil, "")
	if err := auditRepo.Create(ctx, row); err != nil {
		t.Fatalf("audit Create: %v", err)
	}
	logs, err := auditRepo.List(ctx, audit.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("audit List: %v", err)
	}
	if len(logs.Items) == 0 {
		t.Fatal("expected at least one audit log row for tenant")
	}

	// Build the retrieval handler and exercise it end-to-end.
	retrievalHandler, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: qdrant,
		Embedder:    fakeEmbed{},
	})
	if err != nil {
		t.Fatalf("retrieval handler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Next()
	})
	retrievalHandler.Register(&r.RouterGroup)

	reqBody, _ := json.Marshal(retrieval.RetrieveRequest{Query: "hello", TopK: 5})
	req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("retrieve status: %d body=%s", w.Code, w.Body.String())
	}
	var resp retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hits) == 0 {
		t.Fatalf("expected at least one hit, body=%s", w.Body.String())
	}
	if resp.Hits[0].DocumentID != docID {
		t.Fatalf("top hit document_id=%q, want %q", resp.Hits[0].DocumentID, docID)
	}
	if resp.Hits[0].PrivacyLabel != "internal" {
		t.Fatalf("privacy_label=%q want internal", resp.Hits[0].PrivacyLabel)
	}
}
