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
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"gorm.io/gorm"

	"github.com/oklog/ulid/v2"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
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
	// Round-12 Task 4: last_error and last_error_at give operators a
	// per-schedule view into why a tick failed without trawling
	// process logs. last_error is bounded to 1KB (truncated on
	// write) so a stack trace doesn't blow the row size budget.
	LastError   string    `gorm:"type:varchar(1024);column:last_error" json:"last_error,omitempty"`
	LastErrorAt time.Time `gorm:"column:last_error_at" json:"last_error_at,omitempty"`
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
// kill the goroutine. Round-12 Task 4 layered a recover() around
// every tick (see safeTick) so a panic in a child path (e.g. an
// emitter crashing on a malformed Kafka payload) never bricks the
// scheduler goroutine.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	if err := s.SafeTick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.cfg.Logger.Warn("scheduler: tick", slog.String("error", err.Error()))
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.SafeTick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.cfg.Logger.Warn("scheduler: tick", slog.String("error", err.Error()))
			}
		}
	}
}

// SafeTick wraps Tick in a recover() so a panic in the tick path
// is converted into an error rather than killing the goroutine.
// The counter context_engine_scheduler_errors_total is bumped on
// every recovered panic AND every returned error so operators get
// a unified view of scheduler health. Exported as the panic-safe
// public alternative to Tick — callers driving the scheduler from
// their own loop (e.g. e2e tests) should prefer this over Tick.
func (s *Scheduler) SafeTick(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			observability.SchedulerErrorsTotal.Inc()
			stack := string(debug.Stack())
			s.cfg.Logger.Error("scheduler: panic recovered",
				slog.Any("panic", r),
				slog.String("stack", stack),
			)
			err = fmt.Errorf("scheduler: panic recovered: %v", r)
		}
	}()
	if tickErr := s.Tick(ctx); tickErr != nil && !errors.Is(tickErr, context.Canceled) {
		observability.SchedulerErrorsTotal.Inc()
		return tickErr
	} else if tickErr != nil {
		return tickErr
	}
	return nil
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
			observability.SchedulerErrorsTotal.Inc()
			s.recordScheduleError(ctx, row.ID, err, now)
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
				"next_run_at":   next,
				"last_run_at":   now,
				"updated_at":    now,
				"last_error":    "",
				"last_error_at": time.Time{},
			}).Error; err != nil {
			s.cfg.Logger.Warn("scheduler: advance next_run_at",
				slog.String("schedule_id", row.ID),
				slog.String("error", err.Error()),
			)
			observability.SchedulerErrorsTotal.Inc()
			s.recordScheduleError(ctx, row.ID, err, now)
		}
	}
	return nil
}

// recordScheduleError persists last_error / last_error_at on the
// sync_schedules row. The error string is truncated to 1KB so a
// stack trace can't blow up the row. Best-effort: a failure here
// is logged but not surfaced — the in-memory counter and slog
// already captured the original failure.
func (s *Scheduler) recordScheduleError(ctx context.Context, scheduleID string, srcErr error, now time.Time) {
	msg := srcErr.Error()
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	if err := s.cfg.DB.WithContext(ctx).
		Model(&SyncSchedule{}).
		Where("id = ?", scheduleID).
		Updates(map[string]any{
			"last_error":    msg,
			"last_error_at": now,
			"updated_at":    now,
		}).Error; err != nil {
		s.cfg.Logger.Warn("scheduler: persist last_error",
			slog.String("schedule_id", scheduleID),
			slog.String("error", err.Error()),
		)
	}
}

// ErrScheduleValidation is the sentinel returned from UpsertSchedule
// when the caller-supplied schedule fails validation (missing
// tenant/source, unparseable cron expression, cron yields no
// future fire). Callers use errors.Is to map it to HTTP 400
// (caller-side fault), distinguishing it from wrapped GORM /
// driver errors which are server-side and map to 500.
var ErrScheduleValidation = errors.New("scheduler: invalid schedule")

// UpsertSchedule writes a SyncSchedule for (tenantID, sourceID),
// computing a fresh next_run_at from the supplied cron expression.
// It returns the persisted row so the handler can echo the
// computed schedule back to the caller. ParseCron is invoked
// up-front so a bad expression never lands in the DB.
//
// Validation failures (missing tenant/source, bad cron) are
// wrapped with ErrScheduleValidation so the HTTP layer can map
// them to 400. Wrapped GORM errors are returned bare so they map
// to 500 by default.
func UpsertSchedule(ctx context.Context, db *gorm.DB, tenantID, sourceID, expr string, enabled bool, now time.Time) (*SyncSchedule, error) {
	if tenantID == "" || sourceID == "" {
		return nil, fmt.Errorf("%w: missing tenant/source", ErrScheduleValidation)
	}
	cs, err := ParseCron(expr, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScheduleValidation, err)
	}
	next := cs.Next(now)
	if next.IsZero() {
		return nil, fmt.Errorf("%w: cron expression yields no future fire", ErrScheduleValidation)
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
