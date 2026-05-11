// retention_worker.go — Phase 8 / Task 6 background worker that
// enforces tenant + source + namespace retention policies. The
// worker scans the chunks table per tenant, resolves the effective
// MaxAgeDays for each chunk via policy.Effective(), and routes
// expired chunks through the configured RetentionDeleter so the
// vector / graph / metadata tiers all drop the chunk in lockstep.
//
// The worker is opt-in (CONTEXT_ENGINE_RETENTION_INTERVAL on the
// ingest binary) and runs on a long interval (default 1h) so a
// pathological retention rule can't melt the database with a hot
// scan loop.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// ChunkRecord is the projected row the retention worker uses to
// decide whether a chunk has expired. Carrying just the fields the
// worker needs keeps the in-memory footprint of a sweep small.
type ChunkRecord struct {
	ID          string
	TenantID    string
	SourceID    string
	DocumentID  string
	NamespaceID string
	IngestedAt  time.Time
}

// RetentionChunkSource is the read seam over the chunks table. The
// production implementation is RetentionChunkSourceGORM; tests inject
// an in-memory fake.
type RetentionChunkSource interface {
	ListTenants(ctx context.Context) ([]string, error)
	ListChunks(ctx context.Context, tenantID string) ([]ChunkRecord, error)
}

// RetentionPolicySource returns the active rules for tenantID. The
// production implementation queries retention_policies; tests inject
// an in-memory fake.
type RetentionPolicySource interface {
	List(ctx context.Context, tenantID string) ([]policy.RetentionPolicy, error)
}

// RetentionDeleter is the cross-tier delete seam. The worker calls
// DeleteChunk once per expired chunk; production wires a deleter
// that fans out to Postgres, Qdrant, FalkorDB, and Tantivy.
type RetentionDeleter interface {
	DeleteChunk(ctx context.Context, tenantID, documentID, chunkID string) error
}

// RetentionAuditWriter is the narrow contract the retention worker
// uses to emit a chunk.expired audit row per evicted chunk. The
// production wiring passes *audit.Repository; tests inject a
// recorder. Nil disables audit emission so existing deployments
// don't suddenly start writing to the audit log.
type RetentionAuditWriter interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// RetentionWorkerConfig configures a RetentionWorker.
type RetentionWorkerConfig struct {
	Chunks   RetentionChunkSource
	Policies RetentionPolicySource
	Deleter  RetentionDeleter
	Logger   *slog.Logger

	// Interval is the gap between sweeps. Defaults to 1h.
	Interval time.Duration

	// Now is the wall clock used for expiry calculation. Tests
	// inject a deterministic clock; production leaves it nil and the
	// worker uses time.Now.
	Now func() time.Time

	// Audit, when non-nil, receives one chunk.expired event per
	// successfully-deleted chunk so operators can correlate
	// retention activity with the audit timeline. Nil keeps the
	// worker silent (default).
	Audit RetentionAuditWriter

	// Actor is the actor id stamped on the chunk.expired event.
	// Defaults to a system-actor sentinel so the audit reader can
	// distinguish automated retention from user-initiated deletes.
	Actor string
}

// RetentionWorker periodically sweeps chunks and deletes expired
// rows.
type RetentionWorker struct {
	cfg RetentionWorkerConfig
}

// NewRetentionWorker validates cfg and returns a worker.
func NewRetentionWorker(cfg RetentionWorkerConfig) (*RetentionWorker, error) {
	if cfg.Chunks == nil {
		return nil, errors.New("retention: nil Chunks")
	}
	if cfg.Policies == nil {
		return nil, errors.New("retention: nil Policies")
	}
	if cfg.Deleter == nil {
		return nil, errors.New("retention: nil Deleter")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &RetentionWorker{cfg: cfg}, nil
}

// Run executes Sweep on a ticker until ctx is cancelled.
func (w *RetentionWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	if err := w.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.cfg.Logger.Warn("retention: initial sweep", slog.String("error", err.Error()))
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("retention: sweep", slog.String("error", err.Error()))
			}
		}
	}
}

// SweepResult summarises one sweep across all tenants.
type SweepResult struct {
	TenantsScanned int
	ChunksScanned  int
	ChunksExpired  int
	ChunksDeleted  int
}

// Sweep runs a single retention pass across every tenant returned by
// the chunk source. The result is suitable for emitting structured
// log lines.
//
// Round-12 Task 3: every sweep observes the wall-clock duration into
// the `context_engine_retention_sweep_duration_seconds` histogram and
// every successful chunk delete bumps the
// `context_engine_retention_expired_chunks_total` counter (via
// sweepTenant → DeleteChunk on success).
func (w *RetentionWorker) Sweep(ctx context.Context) error {
	start := w.cfg.Now()
	defer func() {
		observability.RetentionSweepDurationSeconds.Observe(w.cfg.Now().Sub(start).Seconds())
	}()
	tenants, err := w.cfg.Chunks.ListTenants(ctx)
	if err != nil {
		return err
	}
	res := SweepResult{}
	for _, tenantID := range tenants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res.TenantsScanned++
		if err := w.sweepTenant(ctx, tenantID, &res); err != nil {
			w.cfg.Logger.Warn("retention: tenant sweep",
				slog.String("tenant_id", tenantID),
				slog.String("error", err.Error()),
			)
			continue
		}
	}
	w.cfg.Logger.Info("retention: sweep complete",
		slog.Int("tenants", res.TenantsScanned),
		slog.Int("chunks_scanned", res.ChunksScanned),
		slog.Int("chunks_expired", res.ChunksExpired),
		slog.Int("chunks_deleted", res.ChunksDeleted),
	)
	return nil
}

func (w *RetentionWorker) sweepTenant(ctx context.Context, tenantID string, res *SweepResult) error {
	rules, err := w.cfg.Policies.List(ctx, tenantID)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}
	chunks, err := w.cfg.Chunks.ListChunks(ctx, tenantID)
	if err != nil {
		return err
	}
	now := w.cfg.Now()
	for _, c := range chunks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res.ChunksScanned++
		eff, ok := policy.Effective(policy.ChunkScope{
			TenantID:    c.TenantID,
			SourceID:    c.SourceID,
			NamespaceID: c.NamespaceID,
		}, rules)
		if !ok {
			continue
		}
		if eff.MaxAgeDays <= 0 {
			continue
		}
		if c.IngestedAt.IsZero() || now.Sub(c.IngestedAt) <= time.Duration(eff.MaxAgeDays)*24*time.Hour {
			continue
		}
		res.ChunksExpired++
		if err := w.cfg.Deleter.DeleteChunk(ctx, c.TenantID, c.DocumentID, c.ID); err != nil {
			w.cfg.Logger.Warn("retention: delete failed",
				slog.String("tenant_id", c.TenantID),
				slog.String("chunk_id", c.ID),
				slog.String("error", err.Error()),
			)
			continue
		}
		res.ChunksDeleted++
		observability.RetentionExpiredChunksTotal.Inc()
		w.emitChunkExpired(ctx, c, eff.MaxAgeDays)
	}
	return nil
}

// emitChunkExpired records a chunk.expired audit event for one
// successfully-deleted chunk. Errors writing the event are logged
// but never block the sweep — the chunk is already gone from the
// storage tier; we'd just rather have a paper trail when we can
// get one.
func (w *RetentionWorker) emitChunkExpired(ctx context.Context, c ChunkRecord, maxAgeDays int) {
	if w.cfg.Audit == nil {
		return
	}
	actor := w.cfg.Actor
	if actor == "" {
		actor = "00000000000000000000000000" // 26-char system actor sentinel
	}
	metadata := map[string]any{
		"document_id":  c.DocumentID,
		"source_id":    c.SourceID,
		"namespace_id": c.NamespaceID,
		"max_age_days": maxAgeDays,
		"ingested_at":  c.IngestedAt.Format(time.RFC3339),
		"chunk_id":     c.ID,
		"deleted_by":   "retention-worker",
	}
	log := audit.NewAuditLog(
		c.TenantID,
		actor,
		audit.ActionChunkExpired,
		"chunk",
		c.ID,
		metadata,
		"",
	)
	if err := w.cfg.Audit.Create(ctx, log); err != nil {
		w.cfg.Logger.Warn("retention: audit write failed",
			slog.String("tenant_id", c.TenantID),
			slog.String("chunk_id", c.ID),
			slog.String("error", err.Error()),
		)
	}
}

// SweepOnce is a thin wrapper that exposes Sweep for tests so they
// can drive a single iteration without a ticker.
func (w *RetentionWorker) SweepOnce(ctx context.Context) (SweepResult, error) {
	res := SweepResult{}
	tenants, err := w.cfg.Chunks.ListTenants(ctx)
	if err != nil {
		return res, err
	}
	for _, tenantID := range tenants {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.TenantsScanned++
		if err := w.sweepTenant(ctx, tenantID, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// RetentionChunkSourceGORM is the production *gorm.DB-backed
// RetentionChunkSource. It only needs access to the chunks table.
type RetentionChunkSourceGORM struct{ db *gorm.DB }

// NewRetentionChunkSourceGORM wires a *gorm.DB to the worker.
func NewRetentionChunkSourceGORM(db *gorm.DB) *RetentionChunkSourceGORM {
	return &RetentionChunkSourceGORM{db: db}
}

// ListTenants returns every distinct tenant_id present in chunks.
func (s *RetentionChunkSourceGORM) ListTenants(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.db.WithContext(ctx).
		Model(&storage.Chunk{}).
		Distinct("tenant_id").
		Pluck("tenant_id", &ids).Error
	return ids, err
}

// ListChunks projects chunks for tenantID into ChunkRecord.
func (s *RetentionChunkSourceGORM) ListChunks(ctx context.Context, tenantID string) ([]ChunkRecord, error) {
	if tenantID == "" {
		return nil, errors.New("retention: missing tenant_id")
	}
	var rows []storage.Chunk
	err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]ChunkRecord, 0, len(rows))
	for _, c := range rows {
		out = append(out, ChunkRecord{
			ID: c.ID, TenantID: c.TenantID, SourceID: c.SourceID,
			DocumentID: c.DocumentID, NamespaceID: c.NamespaceID,
			IngestedAt: c.CreatedAt,
		})
	}
	return out, nil
}

// RetentionPolicySourceGORM is the production *gorm.DB-backed
// RetentionPolicySource.
type RetentionPolicySourceGORM struct{ db *gorm.DB }

// NewRetentionPolicySourceGORM wires a *gorm.DB to the worker.
func NewRetentionPolicySourceGORM(db *gorm.DB) *RetentionPolicySourceGORM {
	return &RetentionPolicySourceGORM{db: db}
}

// AutoMigrate creates the retention_policies table when the SQL
// migration hasn't been applied yet.
func (s *RetentionPolicySourceGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&policy.RetentionPolicy{})
}

// List returns the active rules for tenantID.
func (s *RetentionPolicySourceGORM) List(ctx context.Context, tenantID string) ([]policy.RetentionPolicy, error) {
	if tenantID == "" {
		return nil, errors.New("retention: missing tenant_id")
	}
	var rows []policy.RetentionPolicy
	err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Find(&rows).Error
	return rows, err
}
