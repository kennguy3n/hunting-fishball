//go:build e2e

// Package e2e — round9_pipeline_test.go — Round-10 Task 12.
//
// (a) Drives a coordinator with CONTEXT_ENGINE_PARSE_TIMEOUT=1ms
//     and a parse stage that blocks for 250ms. The per-attempt
//     deadline must fire on every attempt, so after MaxAttempts
//     the event lands in the DLQ.
//
// (b) Drives a retrieval handler with CacheWarmOnMiss=true and
//     a SlowCache whose Set blocks for ~500ms. The HTTP response
//     should return well before the Set finishes; a second call
//     to Get returns the warmed entry.
//
// Both subtests use only public APIs so the fakes live in this
// file; coordinator_test.go's fakes are package-private.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// ---------- (a) per-stage parse-timeout ----------

type pipelineSlowParseFetch struct{}

func (pipelineSlowParseFetch) FetchEvent(_ context.Context, evt pipeline.IngestEvent) (*pipeline.Document, error) {
	return &pipeline.Document{
		TenantID:    evt.TenantID,
		DocumentID:  evt.DocumentID,
		Content:     []byte("hello"),
		ContentHash: "hash-1",
	}, nil
}

type pipelineSlowParse struct {
	delay time.Duration
}

func (p pipelineSlowParse) Parse(ctx context.Context, _ *pipeline.Document) ([]pipeline.Block, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(p.delay):
		return []pipeline.Block{{BlockID: "b1", Text: "ok"}}, nil
	}
}

type pipelineNoopEmbed struct{}

func (pipelineNoopEmbed) EmbedBlocks(_ context.Context, _ string, blocks []pipeline.Block) ([][]float32, string, error) {
	out := make([][]float32, len(blocks))
	for i := range blocks {
		out[i] = []float32{1}
	}
	return out, "model-x", nil
}

type pipelineNoopStore struct{}

func (pipelineNoopStore) Store(_ context.Context, _ *pipeline.Document, _ []pipeline.Block, _ [][]float32, _ string) error {
	return nil
}

func (pipelineNoopStore) Delete(_ context.Context, _, _ string) error { return nil }

// TestRound10_PerStageParseTimeout — Round-10 Task 12 (a).
func TestRound10_PerStageParseTimeout(t *testing.T) {
	t.Parallel()
	cfg := pipeline.CoordinatorConfig{
		Fetch:          pipelineSlowParseFetch{},
		Parse:          pipelineSlowParse{delay: 250 * time.Millisecond},
		Embed:          pipelineNoopEmbed{},
		Store:          pipelineNoopStore{},
		QueueSize:      4,
		MaxAttempts:    2,
		InitialBackoff: time.Microsecond,
		MaxBackoff:     time.Microsecond,
	}
	// LoadStageTimeoutsFromEnv mirrors what cmd/ingest does at
	// startup. The 1ms value is small enough that every parse
	// attempt fails before the 250ms delay completes.
	env := map[string]string{
		"CONTEXT_ENGINE_PARSE_TIMEOUT": "1ms",
	}
	cfg.LoadStageTimeoutsFromEnv(func(k string) string { return env[k] })
	if cfg.ParseTimeout != time.Millisecond {
		t.Fatalf("ParseTimeout: %v", cfg.ParseTimeout)
	}

	var dlqCount atomic.Int32
	var lastErr atomic.Pointer[error]
	cfg.OnDLQ = func(_ context.Context, _ pipeline.IngestEvent, err error) {
		dlqCount.Add(1)
		lastErr.Store(&err)
	}

	coord, err := pipeline.NewCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runCh := make(chan error, 1)
	go func() { runCh <- coord.Run(ctx) }()

	if err := coord.Submit(ctx, pipeline.IngestEvent{
		Kind:       pipeline.EventDocumentChanged,
		TenantID:   "tenant-1",
		SourceID:   "source-1",
		DocumentID: "doc-slow",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if dlqCount.Load() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-runCh

	if got := dlqCount.Load(); got != 1 {
		t.Fatalf("expected 1 DLQ entry; got %d", got)
	}
	if ep := lastErr.Load(); ep == nil || !errors.Is(*ep, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded; got %v", lastErr.Load())
	}
}

// ---------- (b) cache-warm-on-miss ----------

// slowSemanticCache is a public-API fake satisfying retrieval.Cache.
// Set blocks for `delay` after signalling on setStarted; tests use
// the signal to assert the handler returned before the write.
type slowSemanticCache struct {
	mu         sync.Mutex
	delay      time.Duration
	stored     bool
	getResult  *storage.CachedResult
	setStarted chan struct{}
	setDone    chan struct{}
}

func newSlowSemanticCache(d time.Duration) *slowSemanticCache {
	return &slowSemanticCache{
		delay:      d,
		setStarted: make(chan struct{}, 1),
		setDone:    make(chan struct{}, 1),
	}
}

func (c *slowSemanticCache) Get(_ context.Context, _, _ string, _ []float32, _ string) (*storage.CachedResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getResult, nil
}

func (c *slowSemanticCache) Set(_ context.Context, _, _ string, _ []float32, _ string, result *storage.CachedResult, _ time.Duration) error {
	select {
	case c.setStarted <- struct{}{}:
	default:
	}
	time.Sleep(c.delay)
	c.mu.Lock()
	c.getResult = result
	c.stored = true
	c.mu.Unlock()
	select {
	case c.setDone <- struct{}{}:
	default:
	}
	return nil
}

type warmStubVector struct{}

func (warmStubVector) Search(_ context.Context, _ string, _ []float32, _ storage.SearchOpts) ([]storage.QdrantHit, error) {
	return []storage.QdrantHit{{ID: "chunk-1", Score: 0.9, Payload: map[string]any{"tenant_id": "tenant-warm", "title": "doc"}}}, nil
}

type warmStubEmbedder struct{}

func (warmStubEmbedder) EmbedQuery(_ context.Context, _, _ string) ([]float32, error) {
	return []float32{1}, nil
}

// TestRound10_CacheWarmOnMiss_EndToEnd — Round-10 Task 12 (b).
func TestRound10_CacheWarmOnMiss_EndToEnd(t *testing.T) {
	t.Parallel()
	cache := newSlowSemanticCache(500 * time.Millisecond)
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:     warmStubVector{},
		Embedder:        warmStubEmbedder{},
		Cache:           cache,
		CacheWarmOnMiss: true,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-warm")
		c.Next()
	})
	h.Register(&r.RouterGroup)

	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "warm", TopK: 5, PrivacyMode: "secret"})
	t0 := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	elapsed := time.Since(t0)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	// The handler must have returned well before the cache Set
	// finished. We give a 200ms grace window over the 500ms Set
	// delay; in practice the goroutine writes well after the
	// response is flushed.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("handler waited for cache.Set; elapsed=%s", elapsed)
	}
	select {
	case <-cache.setStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("cache.Set never started — warm-on-miss not triggered")
	}
	select {
	case <-cache.setDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("cache.Set never finished")
	}
	// Second request: getResult is now populated so the handler
	// short-circuits on the cache hit (no second cache.Set).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second status: %d body=%s", w2.Code, w2.Body.String())
	}
	cache.mu.Lock()
	stored := cache.stored
	cache.mu.Unlock()
	if !stored {
		t.Fatalf("cache.Set never stored the result")
	}
}
