package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// chunkNamespace seeds the deterministic UUIDv5 we mint for each chunk.
// Qdrant rejects arbitrary strings as point IDs (it accepts only u64
// or UUID), so the (tenant, document, block) composite key is hashed
// into a UUIDv5 derived from this fixed namespace. The result is stable
// across runs, which preserves Stage 4 idempotency.
var chunkNamespace = uuid.MustParse("6f4f3a52-4e8c-4d6d-9f3e-2b6f5a3a1c20")

// chunkID hashes the composite key into a deterministic UUIDv5 used
// both as the chunks.id primary key and as the Qdrant point id.
func chunkID(tenantID, documentID, blockID string) string {
	return uuid.NewSHA1(chunkNamespace, []byte(tenantID+":"+documentID+":"+blockID)).String()
}

// VectorStore is the narrow interface Stage 4 needs from the
// QdrantClient. Tests inject an in-memory fake.
type VectorStore interface {
	EnsureCollection(ctx context.Context, tenantID string) error
	Upsert(ctx context.Context, tenantID string, points []storage.QdrantPoint) error
	Delete(ctx context.Context, tenantID string, ids []string) error
}

// MetadataStore is the narrow interface Stage 4 needs from the
// PostgresStore.
type MetadataStore interface {
	UpsertChunks(ctx context.Context, tenantID string, chunks []storage.Chunk) error
	LatestHashForDocument(ctx context.Context, tenantID, documentID string) (string, error)
	DeleteByDocument(ctx context.Context, tenantID, documentID string) ([]string, error)
}

// StoreConfig configures Stage 4 (Store).
type StoreConfig struct {
	Vector   VectorStore
	Metadata MetadataStore

	// Connector names the source connector for provenance ("google_drive",
	// "slack"). Set per-pipeline by the consumer wiring.
	Connector string

	// Deduplicator, when non-nil and Enabled(), runs a semantic
	// dedup pass over (blocks, embeddings) before the metadata /
	// vector writes (Round-6 Task 2). The dropped chunks emit
	// `chunk.deduplicated` audit entries via the deduplicator's
	// own audit sink.
	Deduplicator *Deduplicator

	// DryRun, when true, skips Stage 4 writes entirely (metadata
	// + vector) and only logs what would have been written
	// (Round-6 Task 15). Pipelines run with DryRun=true return
	// without persisting anything.
	DryRun bool
}

// Storer is the Stage 4 worker.
type Storer struct {
	cfg StoreConfig
}

// NewStorer constructs a Storer.
func NewStorer(cfg StoreConfig) (*Storer, error) {
	if cfg.Vector == nil {
		return nil, errors.New("store: nil VectorStore")
	}
	if cfg.Metadata == nil {
		return nil, errors.New("store: nil MetadataStore")
	}

	return &Storer{cfg: cfg}, nil
}

// Store writes the (blocks, embeddings) pair into Qdrant + PostgreSQL.
// Idempotent: re-running with the same content_hash is a no-op (the
// chunks table primary key absorbs duplicates and the Qdrant Upsert is
// idempotent on point id).
//
// On a content_hash change for an existing document, Stage 4 deletes
// the previous chunks before writing the new ones to keep the storage
// plane in lock step.
func (s *Storer) Store(ctx context.Context, doc *Document, blocks []Block, embeddings [][]float32, modelID string) error {
	if doc == nil {
		return errors.New("store: nil doc")
	}
	if doc.TenantID == "" || doc.DocumentID == "" {
		return fmt.Errorf("%w: tenant_id + document_id required", ErrPoisonMessage)
	}
	if len(blocks) != len(embeddings) {
		return fmt.Errorf("store: block / embedding count mismatch: %d vs %d", len(blocks), len(embeddings))
	}

	// Round-6 Task 2: optional semantic dedup pass before any
	// downstream writes. The deduplicator emits its own audit
	// entries for each suppressed chunk.
	if s.cfg.Deduplicator != nil && s.cfg.Deduplicator.Enabled() {
		res, err := s.cfg.Deduplicator.Apply(ctx, doc, blocks, embeddings)
		if err != nil {
			return fmt.Errorf("store: dedup: %w", err)
		}
		blocks = res.Blocks
		embeddings = res.Embeddings
	}

	// Round-6 Task 15: dry-run mode skips the storage writes
	// entirely. Returning here keeps the pipeline metric +
	// OnSuccess accounting intact.
	if s.cfg.DryRun {
		return nil
	}

	// Make sure the per-tenant collection exists.
	if err := s.cfg.Vector.EnsureCollection(ctx, doc.TenantID); err != nil {
		return fmt.Errorf("store: ensure collection: %w", err)
	}

	// If the document already has chunks under a different content_hash,
	// delete the old ones first. Same hash → idempotent re-write.
	prevHash, err := s.cfg.Metadata.LatestHashForDocument(ctx, doc.TenantID, doc.DocumentID)
	if err != nil {
		return fmt.Errorf("store: latest hash: %w", err)
	}
	if prevHash != "" && prevHash != doc.ContentHash {
		removedIDs, err := s.cfg.Metadata.DeleteByDocument(ctx, doc.TenantID, doc.DocumentID)
		if err != nil {
			return fmt.Errorf("store: delete previous: %w", err)
		}
		if len(removedIDs) > 0 {
			if err := s.cfg.Vector.Delete(ctx, doc.TenantID, removedIDs); err != nil {
				return fmt.Errorf("store: vector delete previous: %w", err)
			}
		}
	}

	// Build the rows.
	chunks := make([]storage.Chunk, len(blocks))
	points := make([]storage.QdrantPoint, len(blocks))
	uri := uriFromMetadata(doc.Metadata)
	for i, b := range blocks {
		id := chunkID(doc.TenantID, doc.DocumentID, b.BlockID)
		chunks[i] = storage.Chunk{
			ID:           id,
			TenantID:     doc.TenantID,
			SourceID:     doc.SourceID,
			DocumentID:   doc.DocumentID,
			NamespaceID:  doc.NamespaceID,
			BlockID:      b.BlockID,
			ContentHash:  doc.ContentHash,
			Title:        doc.Title,
			URI:          uri,
			Connector:    s.cfg.Connector,
			PrivacyLabel: doc.PrivacyLabel,
			Text:         b.Text,
			Model:        modelID,
		}
		payload := map[string]any{
			"document_id":   doc.DocumentID,
			"source_id":     doc.SourceID,
			"namespace_id":  doc.NamespaceID,
			"block_id":      b.BlockID,
			"title":         doc.Title,
			"uri":           uri,
			"connector":     s.cfg.Connector,
			"privacy_label": doc.PrivacyLabel,
			"text":          b.Text,
			"content_hash":  doc.ContentHash,
		}
		points[i] = storage.QdrantPoint{ID: id, Vector: embeddings[i], Payload: payload}
	}

	// Persist metadata first (transactional inside Postgres) so the
	// retrieval API never returns a Qdrant hit without metadata.
	if err := s.cfg.Metadata.UpsertChunks(ctx, doc.TenantID, chunks); err != nil {
		return fmt.Errorf("store: upsert metadata: %w", err)
	}
	if err := s.cfg.Vector.Upsert(ctx, doc.TenantID, points); err != nil {
		return fmt.Errorf("store: upsert vectors: %w", err)
	}

	return nil
}

// Delete is the Stage 4 path for upstream-deletion events: drop every
// chunk for the document from both stores.
func (s *Storer) Delete(ctx context.Context, tenantID, documentID string) error {
	if tenantID == "" || documentID == "" {
		return fmt.Errorf("%w: tenant_id + document_id required", ErrPoisonMessage)
	}
	ids, err := s.cfg.Metadata.DeleteByDocument(ctx, tenantID, documentID)
	if err != nil {
		return fmt.Errorf("store: delete metadata: %w", err)
	}
	if len(ids) > 0 {
		if err := s.cfg.Vector.Delete(ctx, tenantID, ids); err != nil {
			return fmt.Errorf("store: delete vectors: %w", err)
		}
	}

	return nil
}

func uriFromMetadata(md map[string]string) string {
	if md == nil {
		return ""
	}
	if u := md["uri"]; u != "" {
		return u
	}
	if u := md["url"]; u != "" {
		return u
	}

	return md["path"]
}
