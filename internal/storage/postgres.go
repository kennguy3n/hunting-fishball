package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Chunk is the per-block metadata persisted in PostgreSQL alongside
// the embedding written to Qdrant. Indexes are tenant-scoped.
type Chunk struct {
	// ID is the canonical chunk identifier — also used as the Qdrant
	// point ID. The Stage 4 storer mints it as a UUIDv5 derived from
	// the (tenant_id, document_id, block_id) triple so the same ID is
	// stable across runs and acceptable to Qdrant (which only accepts
	// u64 / UUID point ids).
	ID string `gorm:"type:varchar(128);primaryKey;column:id" json:"id"`

	// TenantID is the multi-tenant scope. Indexed.
	TenantID string `gorm:"type:char(26);not null;index;column:tenant_id" json:"tenant_id"`

	// SourceID identifies the connector instance the chunk came from.
	SourceID string `gorm:"type:varchar(128);not null;column:source_id" json:"source_id"`

	// DocumentID is the connector-native document identifier.
	DocumentID string `gorm:"type:varchar(256);not null;column:document_id;index:idx_chunks_doc" json:"document_id"`

	// NamespaceID scopes the chunk inside the source.
	NamespaceID string `gorm:"type:varchar(128);column:namespace_id" json:"namespace_id"`

	// BlockID identifies the parsed block within the document.
	BlockID string `gorm:"type:varchar(64);not null;column:block_id" json:"block_id"`

	// ContentHash keys the idempotency triple. Non-unique because the
	// pipeline writes one chunk per block; the per-document idempotency
	// is enforced by the storage worker before Upsert.
	ContentHash string `gorm:"type:varchar(64);not null;column:content_hash;index" json:"content_hash"`

	// Title and URI surface as provenance in retrieval results.
	Title string `gorm:"type:text;column:title" json:"title"`
	URI   string `gorm:"type:text;column:uri" json:"uri"`

	// Connector names the source connector (e.g. "google-drive", "slack").
	Connector string `gorm:"type:varchar(64);column:connector" json:"connector"`

	// PrivacyLabel surfaces in the retrieval API privacy strip.
	PrivacyLabel string `gorm:"type:varchar(64);column:privacy_label" json:"privacy_label"`

	// Text is the canonical chunk text (Stage 4 persists it so the
	// retrieval API can return it without re-fetching from the source).
	Text string `gorm:"type:text;not null;column:text" json:"text"`

	// Model is the embedding model id Stage 3 used.
	Model string `gorm:"type:varchar(64);column:model" json:"model"`

	// CreatedAt is the wall clock the chunk was first persisted.
	CreatedAt time.Time `gorm:"not null;column:created_at" json:"created_at"`

	// UpdatedAt is bumped on every upsert.
	UpdatedAt time.Time `gorm:"not null;column:updated_at" json:"updated_at"`
}

// TableName overrides the default GORM pluralization.
func (Chunk) TableName() string { return "chunks" }

// PostgresStore persists Chunk metadata. Tenant scope is enforced at
// every read.
type PostgresStore struct {
	db *gorm.DB
}

// NewPostgresStore wires a PostgresStore to the supplied *gorm.DB.
func NewPostgresStore(db *gorm.DB) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("postgres: nil db")
	}

	return &PostgresStore{db: db}, nil
}

// AutoMigrate creates / migrates the chunks table. Used by the e2e
// smoke test and dev fixtures; production migrations live in
// migrations/.
func (s *PostgresStore) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&Chunk{})
}

// UpsertChunks writes a batch of chunks transactionally. The
// (tenant_id, document_id, content_hash) triple is the natural
// idempotency key; Upsert is no-op when the same ID is written twice.
func (s *PostgresStore) UpsertChunks(ctx context.Context, tenantID string, chunks []Chunk) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if len(chunks) == 0 {
		return nil
	}

	for i := range chunks {
		if chunks[i].TenantID != tenantID {
			return fmt.Errorf("postgres: chunk %d tenant mismatch: %q vs %q", i, chunks[i].TenantID, tenantID)
		}
		now := time.Now().UTC()
		if chunks[i].CreatedAt.IsZero() {
			chunks[i].CreatedAt = now
		}
		chunks[i].UpdatedAt = now
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Save() is upsert-by-PK: insert if new, update if exists.
		for i := range chunks {
			if err := tx.Save(&chunks[i]).Error; err != nil {
				return fmt.Errorf("postgres: save chunk %s: %w", chunks[i].ID, err)
			}
		}

		return nil
	})
}

// FetchChunks loads the Chunk records for a list of IDs, refusing
// queries without a tenant_scope and silently dropping rows that
// belong to a different tenant (cross-tenant reads are
// structurally impossible).
func (s *PostgresStore) FetchChunks(ctx context.Context, tenantID string, ids []string) ([]Chunk, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []Chunk
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id IN ?", tenantID, ids).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("postgres: fetch chunks: %w", err)
	}

	return rows, nil
}

// LatestHashForDocument returns the most recent content_hash recorded
// for (tenant_id, document_id), or empty string if no chunks exist
// yet. Stage 4 uses this to short-circuit re-writes when content is
// unchanged.
func (s *PostgresStore) LatestHashForDocument(ctx context.Context, tenantID, documentID string) (string, error) {
	if tenantID == "" {
		return "", ErrMissingTenantScope
	}
	if documentID == "" {
		return "", errors.New("postgres: document_id required")
	}
	var chunk Chunk
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND document_id = ?", tenantID, documentID).
		Order("updated_at DESC").
		First(&chunk).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("postgres: latest hash: %w", err)
	}

	return chunk.ContentHash, nil
}

// DeleteByDocument removes every chunk that belongs to a single
// document. Used by Stage 4 when an upstream delete event fires.
func (s *PostgresStore) DeleteByDocument(ctx context.Context, tenantID, documentID string) ([]string, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}

	// Collect IDs first so the caller can mirror the delete to Qdrant.
	var ids []string
	if err := s.db.WithContext(ctx).
		Model(&Chunk{}).
		Where("tenant_id = ? AND document_id = ?", tenantID, documentID).
		Pluck("id", &ids).Error; err != nil {
		return nil, fmt.Errorf("postgres: collect ids: %w", err)
	}

	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND document_id = ?", tenantID, documentID).
		Delete(&Chunk{}).Error; err != nil {
		return nil, fmt.Errorf("postgres: delete by document: %w", err)
	}

	return ids, nil
}

// DB returns the underlying *gorm.DB. Used by callers that need to
// share a transaction with the audit-log outbox.
func (s *PostgresStore) DB() *gorm.DB { return s.db }
