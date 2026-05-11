// Round-12 Task 17 — audit log retention sweeper.
//
// AuditRetentionSweeper deletes audit_logs rows whose created_at
// is older than the configured retention window. The default
// window is 90 days; override via CONTEXT_ENGINE_AUDIT_RETENTION_DAYS.
//
// Design:
//
//   - Periodic background goroutine launched from cmd/api or
//     cmd/ingest. The Run method takes a context and returns when
//     the context is cancelled.
//   - Each tick runs a batched delete loop: DELETE … LIMIT 1000
//     in a loop until no rows match. This keeps each transaction
//     short and avoids long-running locks on a hot append-only
//     table.
//   - context_engine_audit_rows_expired_total is incremented by
//     the count returned from each DELETE batch.
//   - The cutoff is computed at the top of each sweep, so a
//     long-running sweep doesn't drift the cutoff forward
//     mid-loop.
//
// Database compatibility: the DELETE uses parameterised SQL that
// works on both Postgres and SQLite (used by the unit test). The
// 034_audit_retention.sql migration adds an index on created_at
// to keep the WHERE clause cheap.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// AuditRetentionConfig is the wiring contract for the sweeper.
type AuditRetentionConfig struct {
	// DB is the Postgres / SQLite connection. Required.
	DB *sql.DB

	// RetentionWindow is how long audit_logs rows are kept. Rows
	// with created_at < (now() - RetentionWindow) are deleted.
	// Defaults to 90 days when zero or negative.
	RetentionWindow time.Duration

	// SweepInterval is how often the sweeper wakes. Defaults to
	// 1 hour when zero or negative.
	SweepInterval time.Duration

	// BatchSize is the LIMIT on each DELETE. Defaults to 1000
	// when zero or negative; keeps transactions short on a hot
	// table.
	BatchSize int

	// Logger is the structured logger. nil falls back to slog.Default().
	Logger *slog.Logger

	// Now is injected for tests so the sweep cutoff is deterministic.
	Now func() time.Time
}

// AuditRetentionSweeper runs the periodic delete loop.
type AuditRetentionSweeper struct {
	cfg AuditRetentionConfig
}

// NewAuditRetentionSweeper validates cfg and returns the sweeper.
func NewAuditRetentionSweeper(cfg AuditRetentionConfig) (*AuditRetentionSweeper, error) {
	if cfg.DB == nil {
		return nil, errors.New("audit-retention: nil DB")
	}
	if cfg.RetentionWindow <= 0 {
		cfg.RetentionWindow = 90 * 24 * time.Hour
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = time.Hour
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &AuditRetentionSweeper{cfg: cfg}, nil
}

// Run drives the periodic sweep. Returns when ctx is cancelled.
func (s *AuditRetentionSweeper) Run(ctx context.Context) {
	// Immediate first sweep so freshly-deployed apps don't carry
	// a 1h backlog of expired rows.
	if err := s.Tick(ctx); err != nil {
		s.cfg.Logger.Error("audit-retention: tick", "err", err)
	}
	t := time.NewTicker(s.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				s.cfg.Logger.Error("audit-retention: tick", "err", err)
			}
		}
	}
}

// Tick executes one full sweep — batched deletes until no rows
// match. Returns the total number of deleted rows or the first
// error encountered.
func (s *AuditRetentionSweeper) Tick(ctx context.Context) error {
	cutoff := s.cfg.Now().UTC().Add(-s.cfg.RetentionWindow)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// Use an explicit subquery for SQLite + Postgres
		// compatibility. SQLite does not support LIMIT on DELETE
		// directly, so we DELETE WHERE id IN (SELECT id ... LIMIT N).
		query := `
			DELETE FROM audit_logs
			WHERE id IN (
				SELECT id FROM audit_logs
				WHERE created_at < ?
				LIMIT ?
			)
		`
		res, err := s.cfg.DB.ExecContext(ctx, query, cutoff, s.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("audit-retention: delete: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("audit-retention: rows affected: %w", err)
		}
		total += n
		for i := int64(0); i < n; i++ {
			observability.AuditRowsExpiredTotal.Inc()
		}
		if n == 0 {
			break
		}
	}
	if total > 0 {
		s.cfg.Logger.Info("audit-retention: sweep done",
			"rows_expired", total, "cutoff", cutoff.Format(time.RFC3339))
	}
	return nil
}
