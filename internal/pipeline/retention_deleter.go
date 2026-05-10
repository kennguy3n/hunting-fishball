package pipeline

import (
	"context"
	"errors"
	"fmt"
)

// VectorDeleter is the narrow contract the retention worker needs
// from a vector store. *storage.QdrantClient satisfies it via its
// Delete method.
type VectorDeleter interface {
	Delete(ctx context.Context, tenantID string, ids []string) error
}

// MetadataDeleter is the narrow contract for the chunks-metadata
// store. The worker calls DeleteByDocument once per expired chunk's
// owning document; in practice a chunk and its document are 1:1 in
// the retention sweep contract.
type MetadataDeleter interface {
	DeleteByDocument(ctx context.Context, tenantID, documentID string) ([]string, error)
}

// ComboRetentionDeleter fans a single DeleteChunk call out to the
// metadata + vector tiers. Graph and lexical tiers are not wired
// here yet; the worker logs the per-tier failure and continues.
type ComboRetentionDeleter struct {
	meta   MetadataDeleter
	vector VectorDeleter
}

// NewComboRetentionDeleter validates inputs and returns a deleter.
// Either tier may be nil; a nil tier silently no-ops.
func NewComboRetentionDeleter(meta MetadataDeleter, vector VectorDeleter) *ComboRetentionDeleter {
	return &ComboRetentionDeleter{meta: meta, vector: vector}
}

// DeleteChunk removes the chunk from each configured tier. Vector
// failures don't abort metadata deletes (and vice-versa); the worker
// logs the partial failure and re-tries on the next sweep.
func (d *ComboRetentionDeleter) DeleteChunk(ctx context.Context, tenantID, documentID, chunkID string) error {
	if tenantID == "" {
		return errors.New("retention deleter: missing tenant_id")
	}
	if chunkID == "" {
		return errors.New("retention deleter: missing chunk_id")
	}
	var firstErr error
	if d.vector != nil {
		if err := d.vector.Delete(ctx, tenantID, []string{chunkID}); err != nil {
			firstErr = fmt.Errorf("vector delete: %w", err)
		}
	}
	if d.meta != nil && documentID != "" {
		if _, err := d.meta.DeleteByDocument(ctx, tenantID, documentID); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("metadata delete: %w", err)
		}
	}
	return firstErr
}
