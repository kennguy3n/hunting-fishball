//go:build e2e

// Phase 6 device-first e2e surface. Indexes one document through
// the same fake-pipeline-into-real-storage path smoke_test.go uses
// then submits two POST /v1/retrieve requests with different
// device_tier values, asserting the prefer_local + reason fields
// on the response match the contract documented in
// `internal/policy/device_first.go`.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// TestSmoke_DeviceFirstHint indexes one document and then submits
// two retrieval requests:
//
//  1. device_tier=high — eligible for prefer_local. The Phase 5
//     shard manifest seeded below makes the response say
//     prefer_local=true with reason="prefer_local".
//  2. device_tier=low — ineligible. The response carries
//     prefer_local=false with reason="device_tier_too_low".
//
// Both branches are covered by unit tests in
// `internal/policy/device_first_test.go`; this test exists to
// catch wiring regressions where the gin handler forgets to
// populate the inputs from the request envelope.
func TestSmoke_DeviceFirstHint(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)

	// Phase 1 storage plane: stand up postgres + qdrant the same
	// way smoke_test.go does. We only need a single document to
	// verify the response surface populates prefer_local.
	pgStore, err := storage.NewPostgresStore(db)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	if err := pgStore.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	qdrant, err := storage.NewQdrantClient(storage.QdrantConfig{
		BaseURL:    envOr("CONTEXT_ENGINE_QDRANT_URL", "http://localhost:6333"),
		VectorSize: 8,
	})
	if err != nil {
		t.Fatalf("NewQdrantClient: %v", err)
	}

	storer, err := pipeline.NewStorer(pipeline.StoreConfig{
		Vector:    qdrant,
		Metadata:  pgStore,
		Connector: "device-first-test",
	})
	if err != nil {
		t.Fatalf("NewStorer: %v", err)
	}
	coord, err := pipeline.NewCoordinator(pipeline.CoordinatorConfig{
		Fetch: fakeFetch{}, Parse: fakeParse{}, Embed: fakeEmbed{}, Store: storer,
		QueueSize: 4, MaxAttempts: 1,
		InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}

	docID := fmt.Sprintf("doc-%d", time.Now().UnixNano())
	runDone := make(chan error, 1)
	go func() { runDone <- coord.Run(ctx) }()
	if err := coord.Submit(ctx, pipeline.IngestEvent{
		Kind:          pipeline.EventDocumentChanged,
		TenantID:      tenantID,
		SourceID:      "device-first",
		DocumentID:    docID,
		NamespaceID:   "ns",
		Title:         "device-first-doc",
		MIMEType:      "text/plain",
		PrivacyLabel:  "internal",
		InlineContent: []byte("device first hint payload"),
		Metadata:      map[string]string{"uri": "smoke://df"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	coord.CloseInputs()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Seed a shard manifest so the device-first policy returns
	// prefer_local=true (it requires LocalShardVersion > 0).
	if err := shard.NewRepository(db).Create(ctx, &shard.ShardManifest{
		TenantID:     tenantID,
		PrivacyMode:  "local-only",
		ShardVersion: 1,
		ChunksCount:  1,
		Status:       shard.ShardStatusReady,
	}); err != nil {
		t.Fatalf("seed shard: %v", err)
	}

	handler, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        qdrant,
		Embedder:           fakeEmbed{},
		ShardVersionLookup: shard.VersionLookup{Repo: shard.NewRepository(db)},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Next()
	})
	handler.Register(&r.RouterGroup)

	// (1) High tier → prefer_local=true.
	resp := postRetrieve(t, r, retrieval.RetrieveRequest{
		Query:       "device first hint",
		TopK:        5,
		DeviceTier:  "high",
		PrivacyMode: "local-only",
	})
	if !resp.PreferLocal {
		t.Errorf("high tier: prefer_local=false reason=%q want true", resp.PreferLocalReason)
	}
	if resp.PreferLocalReason != "prefer_local" {
		t.Errorf("high tier reason=%q want prefer_local", resp.PreferLocalReason)
	}

	// (2) Low tier → prefer_local=false with device_tier_too_low.
	resp = postRetrieve(t, r, retrieval.RetrieveRequest{
		Query:       "device first hint",
		TopK:        5,
		DeviceTier:  "low",
		PrivacyMode: "local-only",
	})
	if resp.PreferLocal {
		t.Errorf("low tier: prefer_local=true want false")
	}
	if resp.PreferLocalReason != "device_tier_too_low" {
		t.Errorf("low tier reason=%q want device_tier_too_low", resp.PreferLocalReason)
	}
}

// postRetrieve is a small helper that POSTs a RetrieveRequest
// JSON-encoded and decodes the response into a RetrieveResponse.
// Centralising the marshal/unmarshal keeps the tests readable.
func postRetrieve(t *testing.T, r *gin.Engine, req retrieval.RetrieveRequest) retrieval.RetrieveResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	httpReq, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("retrieve status=%d body=%s", w.Code, w.Body.String())
	}
	var resp retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}
