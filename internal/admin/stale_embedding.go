// stale_embedding.go — Round-18 Task 11.
//
// Embedding-model versioning. When a tenant changes their
// embedding model (PUT /v1/admin/sources/:id/embedding), every
// chunk in Qdrant that was embedded with the previous model is
// stale: its vector is in the wrong embedding space relative to
// the new query embeddings, which destroys retrieval quality.
//
// This file owns:
//
//   - The ChunkEmbeddingVersion GORM model mirroring migration
//     041_chunk_embedding_version.sql.
//   - StaleEmbeddingDetector — marks chunks whose recorded model
//     differs from the tenant's current embedding config.
//   - StaleEmbeddingWorker — background loop that drains stale
//     rows via a pluggable ReEmbedFn (the production binding wires
//     this to pipeline.RunStage3 for the chunk).
//
// The worker is intentionally cooperative: it processes a small
// batch per tick, surfaces last_error/attempts on the row, and
// stops the moment the row is no longer stale (the detector's
// clearance writeback resets the marker).

package admin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
)

// ChunkEmbeddingVersion is the GORM model for
// chunk_embedding_version (migration 041).
type ChunkEmbeddingVersion struct {
	TenantID            string     `gorm:"type:char(26);primaryKey;column:tenant_id" json:"tenant_id"`
	ChunkID             string     `gorm:"type:varchar(64);primaryKey;column:chunk_id" json:"chunk_id"`
	EmbeddingModelID    string     `gorm:"type:varchar(128);not null;column:embedding_model_id" json:"embedding_model_id"`
	EmbeddingDimensions int        `gorm:"column:embedding_dimensions;not null;default:0" json:"embedding_dimensions"`
	EmbeddedAt          time.Time  `gorm:"not null;default:now();column:embedded_at" json:"embedded_at"`
	StaleSince          *time.Time `gorm:"column:stale_since" json:"stale_since,omitempty"`
	LastAttemptAt       *time.Time `gorm:"column:last_attempt_at" json:"last_attempt_at,omitempty"`
	LastError           string     `gorm:"type:text;column:last_error" json:"last_error,omitempty"`
	Attempts            int        `gorm:"column:attempts;not null;default:0" json:"attempts"`
}

// TableName overrides GORM pluralisation.
func (ChunkEmbeddingVersion) TableName() string { return "chunk_embedding_version" }

// ChunkEmbeddingVersionStore is the read/write port. The
// background worker and the detector both go through it so an
// in-memory fake makes unit tests possible without a Postgres.
type ChunkEmbeddingVersionStore interface {
	// MarkStale tags every (tenant, chunk_id) row whose
	// embedding_model_id != currentModelID as stale_since=now().
	// Returns the number of rows marked.
	MarkStale(ctx context.Context, tenantID, currentModelID string, now time.Time) (int, error)

	// ListStale returns up to `limit` stale rows for the tenant,
	// oldest stale_since first.
	ListStale(ctx context.Context, tenantID string, limit int) ([]*ChunkEmbeddingVersion, error)

	// RecordReembed writes the new model id+dimensions and clears
	// stale_since on success.
	RecordReembed(ctx context.Context, row *ChunkEmbeddingVersion, modelID string, dimensions int, now time.Time) error

	// RecordFailure bumps attempts and stamps last_error / last_attempt_at.
	RecordFailure(ctx context.Context, row *ChunkEmbeddingVersion, errMsg string, now time.Time) error
}

// GormChunkEmbeddingVersionStore is the Postgres-backed store.
type GormChunkEmbeddingVersionStore struct {
	db *gorm.DB
}

// NewGormChunkEmbeddingVersionStore wires the store.
func NewGormChunkEmbeddingVersionStore(db *gorm.DB) *GormChunkEmbeddingVersionStore {
	return &GormChunkEmbeddingVersionStore{db: db}
}

// MarkStale implements ChunkEmbeddingVersionStore.
func (s *GormChunkEmbeddingVersionStore) MarkStale(ctx context.Context, tenantID, currentModelID string, now time.Time) (int, error) {
	if tenantID == "" || currentModelID == "" {
		return 0, errors.New("admin: tenant_id and current model id required")
	}
	res := s.db.WithContext(ctx).
		Model(&ChunkEmbeddingVersion{}).
		Where("tenant_id = ? AND embedding_model_id <> ? AND stale_since IS NULL", tenantID, currentModelID).
		Update("stale_since", now)
	if res.Error != nil {
		return 0, fmt.Errorf("admin: mark stale: %w", res.Error)
	}

	return int(res.RowsAffected), nil
}

// ListStale implements ChunkEmbeddingVersionStore.
func (s *GormChunkEmbeddingVersionStore) ListStale(ctx context.Context, tenantID string, limit int) ([]*ChunkEmbeddingVersion, error) {
	if limit <= 0 {
		limit = 50
	}
	out := []*ChunkEmbeddingVersion{}
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND stale_since IS NOT NULL", tenantID).
		Order("stale_since ASC").
		Limit(limit).
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("admin: list stale: %w", err)
	}

	return out, nil
}

// RecordReembed implements ChunkEmbeddingVersionStore.
func (s *GormChunkEmbeddingVersionStore) RecordReembed(ctx context.Context, row *ChunkEmbeddingVersion, modelID string, dimensions int, now time.Time) error {
	if row == nil {
		return errors.New("admin: nil row")
	}
	updates := map[string]any{
		"embedding_model_id":   modelID,
		"embedding_dimensions": dimensions,
		"embedded_at":          now,
		"stale_since":          nil,
		"last_error":           "",
	}

	return s.db.WithContext(ctx).
		Model(&ChunkEmbeddingVersion{}).
		Where("tenant_id = ? AND chunk_id = ?", row.TenantID, row.ChunkID).
		Updates(updates).Error
}

// RecordFailure implements ChunkEmbeddingVersionStore.
func (s *GormChunkEmbeddingVersionStore) RecordFailure(ctx context.Context, row *ChunkEmbeddingVersion, errMsg string, now time.Time) error {
	if row == nil {
		return errors.New("admin: nil row")
	}

	return s.db.WithContext(ctx).
		Model(&ChunkEmbeddingVersion{}).
		Where("tenant_id = ? AND chunk_id = ?", row.TenantID, row.ChunkID).
		Updates(map[string]any{
			"attempts":        gorm.Expr("attempts + 1"),
			"last_attempt_at": now,
			"last_error":      errMsg,
		}).Error
}

// StaleEmbeddingDetector compares each tenant's current embedding
// model against the recorded model on every chunk and stamps
// stale_since on any divergence.
type StaleEmbeddingDetector struct {
	store ChunkEmbeddingVersionStore
	cfg   EmbeddingConfigGetter
}

// EmbeddingConfigGetter is the narrow read interface the detector
// needs from the embedding config store. The production wiring
// passes *EmbeddingConfigRepository.
type EmbeddingConfigGetter interface {
	Get(ctx context.Context, tenantID, sourceID string) (*SourceEmbeddingConfig, error)
}

// NewStaleEmbeddingDetector wires the detector.
func NewStaleEmbeddingDetector(store ChunkEmbeddingVersionStore, cfg EmbeddingConfigGetter) *StaleEmbeddingDetector {
	return &StaleEmbeddingDetector{store: store, cfg: cfg}
}

// DetectForSource compares the tenant/source's current embedding
// model against recorded chunk versions and stamps stale_since on
// any divergence. Returns the number of rows marked stale.
func (d *StaleEmbeddingDetector) DetectForSource(ctx context.Context, tenantID, sourceID string, now time.Time) (int, error) {
	cfg, err := d.cfg.Get(ctx, tenantID, sourceID)
	if errors.Is(err, ErrEmbeddingConfigNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	return d.store.MarkStale(ctx, tenantID, cfg.ModelName, now)
}

// ReEmbedFn is the pluggable hot path for the worker. The
// production binding wires this to Stage 3 (Embed) for the chunk.
// Returns the (modelID, dimensions) used for the new vector.
type ReEmbedFn func(ctx context.Context, row *ChunkEmbeddingVersion) (modelID string, dimensions int, err error)

// StaleEmbeddingWorkerConfig configures the background loop.
type StaleEmbeddingWorkerConfig struct {
	Store     ChunkEmbeddingVersionStore
	ReEmbed   ReEmbedFn
	Tenants   func(ctx context.Context) ([]string, error)
	Interval  time.Duration
	BatchSize int
	Now       func() time.Time
}

// StaleEmbeddingWorker drains stale chunks.
type StaleEmbeddingWorker struct {
	cfg  StaleEmbeddingWorkerConfig
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewStaleEmbeddingWorker wires the worker.
func NewStaleEmbeddingWorker(cfg StaleEmbeddingWorkerConfig) *StaleEmbeddingWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &StaleEmbeddingWorker{cfg: cfg, stop: make(chan struct{})}
}

// Start kicks off the background loop. Stop() blocks until the
// loop exits.
func (w *StaleEmbeddingWorker) Start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stop:
				return
			case <-t.C:
				_ = w.RunOnce(ctx)
			}
		}
	}()
}

// Stop halts the worker.
func (w *StaleEmbeddingWorker) Stop() {
	close(w.stop)
	w.wg.Wait()
}

// RunOnce processes one batch per tenant. Exported so tests can
// drive the worker deterministically.
func (w *StaleEmbeddingWorker) RunOnce(ctx context.Context) error {
	if w.cfg.Tenants == nil || w.cfg.Store == nil || w.cfg.ReEmbed == nil {
		return errors.New("admin: stale-embedding worker missing wiring")
	}
	tenants, err := w.cfg.Tenants(ctx)
	if err != nil {
		return fmt.Errorf("admin: list tenants: %w", err)
	}
	for _, tenantID := range tenants {
		rows, err := w.cfg.Store.ListStale(ctx, tenantID, w.cfg.BatchSize)
		if err != nil {
			continue
		}
		for _, row := range rows {
			model, dim, rerr := w.cfg.ReEmbed(ctx, row)
			now := w.cfg.Now()
			if rerr != nil {
				_ = w.cfg.Store.RecordFailure(ctx, row, rerr.Error(), now)
				continue
			}
			_ = w.cfg.Store.RecordReembed(ctx, row, model, dim, now)
		}
	}

	return nil
}
