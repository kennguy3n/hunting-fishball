// Source-sync cron scheduler.
//
// The scheduler is a single goroutine that wakes up every
// configurable interval (default 1 minute), reads every enabled
// SyncSchedule whose `next_run_at` is in the past, advances
// next_run_at, and emits a Kafka ingest event so the existing
// pipeline picks the source up.
//
// The model is per (tenant_id, source_id) — admins manage the
// schedule via POST /v1/admin/sources/:id/schedule (upsert) and
// GET /v1/admin/sources/:id/schedule (read).
package admin

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/oklog/ulid/v2"
)

// SyncSchedule is the GORM model for sync_schedules. Schema lives
// in migrations/013_sync_schedules.sql; the model omits GORM
// `index:` tags so AutoMigrate cannot drift from the migration.
type SyncSchedule struct {
	ID        string    `gorm:"type:varchar(26);primaryKey;column:id" json:"id"`
	TenantID  string    `gorm:"type:varchar(26);not null;column:tenant_id" json:"tenant_id"`
	SourceID  string    `gorm:"type:varchar(26);not null;column:source_id" json:"source_id"`
	CronExpr  string    `gorm:"type:varchar(64);not null;column:cron_expr" json:"cron_expr"`
	NextRunAt time.Time `gorm:"not null;column:next_run_at" json:"next_run_at"`
	Enabled   bool      `gorm:"not null;column:enabled" json:"enabled"`
	LastRunAt time.Time `gorm:"column:last_run_at" json:"last_run_at,omitempty"`
	CreatedAt time.Time `gorm:"not null;column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null;column:updated_at" json:"updated_at"`
}

// TableName overrides the default GORM pluralisation.
func (SyncSchedule) TableName() string { return "sync_schedules" }

// SyncEmitter is the narrow seam the scheduler uses to push an
// ingest event to Kafka (or whatever transport the consumer is
// using). The production wiring passes a closure around
// pipeline.Producer; tests inject a recorder.
type SyncEmitter interface {
	EmitSync(ctx context.Context, tenantID, sourceID string) error
}

// SyncEmitterFunc adapts a plain function to SyncEmitter.
type SyncEmitterFunc func(ctx context.Context, tenantID, sourceID string) error

// EmitSync implements SyncEmitter.
func (f SyncEmitterFunc) EmitSync(ctx context.Context, tenantID, sourceID string) error {
	return f(ctx, tenantID, sourceID)
}

// SchedulerConfig wires a Scheduler.
type SchedulerConfig struct {
	DB       *gorm.DB
	Emitter  SyncEmitter
	Logger   *slog.Logger
	Interval time.Duration
	Now      func() time.Time
}

// Scheduler runs Tick on a wall-clock ticker until ctx is
// cancelled.
type Scheduler struct {
	cfg SchedulerConfig
}

// NewScheduler validates cfg and returns a scheduler.
func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.DB == nil {
		return nil, errors.New("scheduler: nil DB")
	}
	if cfg.Emitter == nil {
		return nil, errors.New("scheduler: nil Emitter")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Scheduler{cfg: cfg}, nil
}

// AutoMigrate is a thin wrapper for the DI container.
func (s *Scheduler) AutoMigrate(ctx context.Context) error {
	return s.cfg.DB.WithContext(ctx).AutoMigrate(&SyncSchedule{})
}

// Run executes Tick on a ticker until ctx is cancelled. Errors are
// logged but never propagate so a transient DB outage does not
// kill the goroutine.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	if err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.cfg.Logger.Warn("scheduler: tick", slog.String("error", err.Error()))
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.cfg.Logger.Warn("scheduler: tick", slog.String("error", err.Error()))
			}
		}
	}
}

// Tick runs a single scheduling pass: load every enabled schedule
// whose next_run_at is due, emit a sync event, and advance
// next_run_at. Exposed for tests.
func (s *Scheduler) Tick(ctx context.Context) error {
	now := s.cfg.Now()
	var due []SyncSchedule
	if err := s.cfg.DB.WithContext(ctx).
		Where("enabled = ? AND next_run_at <= ?", true, now).
		Order("next_run_at").
		Find(&due).Error; err != nil {
		return err
	}
	for i := range due {
		row := due[i]
		schedule, err := ParseCron(row.CronExpr, time.UTC)
		if err != nil {
			s.cfg.Logger.Warn("scheduler: bad cron expression",
				slog.String("schedule_id", row.ID),
				slog.String("expr", row.CronExpr),
				slog.String("error", err.Error()),
			)
			continue
		}
		if err := s.cfg.Emitter.EmitSync(ctx, row.TenantID, row.SourceID); err != nil {
			s.cfg.Logger.Warn("scheduler: emit failed",
				slog.String("schedule_id", row.ID),
				slog.String("error", err.Error()),
			)
			continue
		}
		next := schedule.Next(now)
		if next.IsZero() {
			s.cfg.Logger.Warn("scheduler: cron yields no future fire",
				slog.String("expr", row.CronExpr),
			)
			continue
		}
		if err := s.cfg.DB.WithContext(ctx).
			Model(&SyncSchedule{}).
			Where("id = ?", row.ID).
			Updates(map[string]any{
				"next_run_at": next,
				"last_run_at": now,
				"updated_at":  now,
			}).Error; err != nil {
			s.cfg.Logger.Warn("scheduler: advance next_run_at",
				slog.String("schedule_id", row.ID),
				slog.String("error", err.Error()),
			)
		}
	}
	return nil
}

// UpsertSchedule writes a SyncSchedule for (tenantID, sourceID),
// computing a fresh next_run_at from the supplied cron expression.
// It returns the persisted row so the handler can echo the
// computed schedule back to the caller. ParseCron is invoked
// up-front so a bad expression never lands in the DB.
func UpsertSchedule(ctx context.Context, db *gorm.DB, tenantID, sourceID, expr string, enabled bool, now time.Time) (*SyncSchedule, error) {
	if tenantID == "" || sourceID == "" {
		return nil, errors.New("scheduler: missing tenant/source")
	}
	cs, err := ParseCron(expr, time.UTC)
	if err != nil {
		return nil, err
	}
	next := cs.Next(now)
	if next.IsZero() {
		return nil, errors.New("scheduler: cron expression yields no future fire")
	}

	row := SyncSchedule{
		TenantID:  tenantID,
		SourceID:  sourceID,
		CronExpr:  expr,
		NextRunAt: next,
		Enabled:   enabled,
		UpdatedAt: now,
	}
	// Upsert by (tenant_id, source_id) — re-using the same source
	// id replaces the schedule wholesale rather than introducing
	// duplicates.
	var existing SyncSchedule
	err = db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		row.ID = ulid.Make().String()
		row.CreatedAt = now
		if err := db.WithContext(ctx).Create(&row).Error; err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		if err := db.WithContext(ctx).
			Model(&SyncSchedule{}).
			Where("id = ?", existing.ID).
			Updates(map[string]any{
				"cron_expr":   expr,
				"next_run_at": next,
				"enabled":     enabled,
				"updated_at":  now,
			}).Error; err != nil {
			return nil, err
		}
	}
	return &row, nil
}

// GetSchedule reads the SyncSchedule for (tenantID, sourceID).
// Returns gorm.ErrRecordNotFound when no schedule exists.
func GetSchedule(ctx context.Context, db *gorm.DB, tenantID, sourceID string) (*SyncSchedule, error) {
	var row SyncSchedule
	if err := db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}
