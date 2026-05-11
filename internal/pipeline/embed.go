package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	embeddingv1 "github.com/kennguy3n/hunting-fishball/proto/embedding/v1"
)

// EmbedConfig configures Stage 3 (Embed).
type EmbedConfig struct {
	// Local is the gRPC client for the local Python embedding service.
	// Required.
	Local embeddingv1.EmbeddingServiceClient

	// Remote, when non-nil, is consulted *before* Local for tenants
	// whose policy allows remote embedding (per ARCHITECTURE.md §3.3,
	// "Per-tenant config picks the path"). The default routes
	// everything to Local.
	Remote RemoteEmbedder

	// AllowRemote returns true when the tenant's policy permits
	// routing to the remote API. Defaults to "always false" — the
	// production wiring binds this to the per-tenant policy lookup.
	AllowRemote func(tenantID string) bool

	// BatchSize bounds the chunks per gRPC call. Defaults to 32.
	BatchSize int

	// Timeout caps each ComputeEmbeddings call. Defaults to 30s.
	Timeout time.Duration

	// MaxAttempts bounds the retry attempts. Defaults to 3.
	MaxAttempts int

	// InitialBackoff is the first retry sleep. Defaults to 200ms.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential retry backoff. Defaults to 5s.
	MaxBackoff time.Duration

	// ModelID, when non-empty, is sent in every request as the
	// preferred model. Empty leaves the choice to the embedding
	// service.
	ModelID string

	// ConfigResolver, when non-nil, is consulted before every
	// EmbedBlocksForSource call to look up a per-source model
	// override (Round-6 Task 1 / Round-8 Task 3). The resolver
	// returns ("", 0) when no row exists and the embed stage falls
	// back to ModelID.
	ConfigResolver EmbeddingConfigResolver
}

// EmbeddingConfigResolver is the narrow port the embed stage uses
// to look up per-source model overrides. The production wiring
// passes admin.EmbeddingConfigRepository; tests inject a fake.
type EmbeddingConfigResolver interface {
	ResolveEmbeddingModel(ctx context.Context, tenantID, sourceID string) (model string, dimensions int)
}

// RemoteEmbedder is the abstraction over remote embedding APIs (OpenAI,
// Voyage, Cohere, ...). The Phase 1 wiring uses an HTTP client behind
// this interface; tests inject an in-memory fake.
type RemoteEmbedder interface {
	Embed(ctx context.Context, tenantID string, chunks []string) ([][]float32, string, error)
}

func (c *EmbedConfig) defaults() {
	if c.BatchSize == 0 {
		c.BatchSize = 32
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.InitialBackoff == 0 {
		c.InitialBackoff = 200 * time.Millisecond
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 5 * time.Second
	}
	if c.AllowRemote == nil {
		c.AllowRemote = func(string) bool { return false }
	}
}

// Embedder is the Stage 3 worker.
type Embedder struct {
	cfg EmbedConfig
}

// NewEmbedder constructs an Embedder. Returns an error if Local is nil
// (the local path is the always-available fallback).
func NewEmbedder(cfg EmbedConfig) (*Embedder, error) {
	if cfg.Local == nil {
		return nil, errors.New("embed: nil Local client")
	}
	cfg.defaults()

	return &Embedder{cfg: cfg}, nil
}

// EmbedBlocks runs Stage 3 over the supplied parsed blocks. The blocks
// are batched into BatchSize-sized requests; the result preserves
// block order so the caller can zip embeddings back into blocks.
//
// Returns the per-block embedding plus the model id the service
// reported (so Stage 4 can persist it as part of the chunk metadata).
func (e *Embedder) EmbedBlocks(ctx context.Context, tenantID string, blocks []Block) ([][]float32, string, error) {
	return e.EmbedBlocksForSource(ctx, tenantID, "", blocks)
}

// EmbedBlocksForSource is the per-source variant: when ConfigResolver
// is set, the resolver picks the model for (tenantID, sourceID); the
// chosen model overrides cfg.ModelID for this call only. Falls back
// to cfg.ModelID when the resolver returns "" / is nil.
func (e *Embedder) EmbedBlocksForSource(ctx context.Context, tenantID, sourceID string, blocks []Block) ([][]float32, string, error) {
	chunks := make([]string, len(blocks))
	for i, b := range blocks {
		chunks[i] = b.Text
	}
	modelID := e.cfg.ModelID
	if e.cfg.ConfigResolver != nil && sourceID != "" {
		if m, _ := e.cfg.ConfigResolver.ResolveEmbeddingModel(ctx, tenantID, sourceID); m != "" {
			modelID = m
		}
	}

	return e.embedWithModel(ctx, tenantID, chunks, modelID)
}

// EmbedTexts runs Stage 3 over a flat slice of texts (used by the
// retrieval API to embed the user query).
func (e *Embedder) EmbedTexts(ctx context.Context, tenantID string, chunks []string) ([][]float32, string, error) {
	return e.embedWithModel(ctx, tenantID, chunks, e.cfg.ModelID)
}

func (e *Embedder) embedWithModel(ctx context.Context, tenantID string, chunks []string, modelID string) ([][]float32, string, error) {
	if len(chunks) == 0 {
		return nil, "", nil
	}

	if e.cfg.Remote != nil && e.cfg.AllowRemote(tenantID) {
		out, model, err := e.cfg.Remote.Embed(ctx, tenantID, chunks)
		if err == nil {
			return out, model, nil
		}
		// Fall through to local on remote failure: the local path
		// always works for tenants with a deployed embedding service.
	}

	out := make([][]float32, 0, len(chunks))
	model := ""
	for start := 0; start < len(chunks); start += e.cfg.BatchSize {
		end := start + e.cfg.BatchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[start:end]
		em, m, err := e.callLocal(ctx, tenantID, batch, modelID)
		if err != nil {
			return nil, "", err
		}
		if model == "" {
			model = m
		}
		out = append(out, em...)
	}

	return out, model, nil
}

// callLocal does one gRPC call with retry / timeout. modelID is the
// per-call override (cfg.ModelID by default; per-source resolver may
// substitute).
func (e *Embedder) callLocal(ctx context.Context, tenantID string, batch []string, modelID string) ([][]float32, string, error) {
	req := &embeddingv1.ComputeEmbeddingsRequest{
		TenantId: tenantID,
		Chunks:   batch,
		ModelId:  modelID,
	}
	var lastErr error
	backoff := e.cfg.InitialBackoff

	for attempt := 1; attempt <= e.cfg.MaxAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
		resp, err := e.cfg.Local.ComputeEmbeddings(callCtx, req)
		cancel()
		if err == nil {
			out := make([][]float32, 0, len(resp.GetEmbeddings()))
			for _, em := range resp.GetEmbeddings() {
				out = append(out, append([]float32(nil), em.GetValues()...))
			}

			return out, resp.GetModelId(), nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, "", fmt.Errorf("embed: %w", err)
		}
		if attempt == e.cfg.MaxAttempts {
			break
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()

			return nil, "", ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > e.cfg.MaxBackoff {
			backoff = e.cfg.MaxBackoff
		}
	}

	return nil, "", fmt.Errorf("embed: exhausted retries: %w", lastErr)
}
