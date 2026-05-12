// api_key_sweeper.go — Round-14 Task 7.
//
// Background worker that transitions API-key rows out of the
// `grace` state once their grace_until window has elapsed.
//
// Round-13 Task 10 added the rotation endpoint that flips the
// previously-active row to `grace` with a configurable
// `grace_until`. Nothing was sweeping those rows once the
// window expired, so a stale `grace` key could still be
// accepted indefinitely if a misconfigured grace_until landed
// in the future. The sweeper closes that loop:
//
//   * Every CONTEXT_ENGINE_API_KEY_SWEEP_INTERVAL ticks
//     (default 5m), select rows WHERE status='grace' AND
//     grace_until < now() and UPDATE status='expired'.
//   * Increment context_engine_api_keys_expired_total for every
//     row transitioned.
//   * Emit an api_key.expired audit event per row so the
//     transition is captured in the append-only log.
//
// `expired` is a NEW value of the api_keys.status enum. The
// existing application enum constants (active/grace/revoked)
// remain — `expired` is functionally identical to `revoked`
// from the request-acceptance side; we keep the codes separate
// so operators can distinguish manual revocations from
// natural grace-period expiry.
package admin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// APIKeyStatusExpired is the post-sweep terminal state. Stays
// internal-only because both `revoked` and `expired` collapse to
// "reject" at the request authentication layer.
const APIKeyStatusExpired APIKeyStatus = "expired"

// APIKeyExpirer is the narrow write seam used by the sweeper.
// The Postgres-backed store and an in-memory test fake both
// satisfy it.
type APIKeyExpirer interface {
	ExpireGrace(ctx context.Context, now time.Time) ([]*APIKeyRow, error)
}

// ExpireGrace moves every grace row whose grace_until < now to
// `expired` and returns the affected rows. The store implements
// this for production; the test fake re-implements it directly.
func (s *APIKeyStoreGORM) ExpireGrace(ctx context.Context, now time.Time) ([]*APIKeyRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("api_keys: nil store")
	}
	var affected []*APIKeyRow
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Read first so we can return the rows for the caller's
		// audit emission. The UPDATE then runs against the same
		// predicate in the same tx.
		if err := tx.Where(
			"status = ? AND grace_until IS NOT NULL AND grace_until < ?",
			string(APIKeyStatusGrace), now,
		).Find(&affected).Error; err != nil {
			return err
		}
		if len(affected) == 0 {
			return nil
		}
		return tx.Model(&APIKeyRow{}).
			Where("status = ? AND grace_until IS NOT NULL AND grace_until < ?",
				string(APIKeyStatusGrace), now).
			Update("status", string(APIKeyStatusExpired)).Error
	})
	if err != nil {
		return nil, err
	}
	return affected, nil
}

// APIKeySweeper transitions expired grace rows on a periodic
// timer. The worker is process-local; cmd/api owns the lifecycle.
type APIKeySweeper struct {
	store    APIKeyExpirer
	audit    AuditRecorder
	interval time.Duration
	nowFn    func() time.Time
}

// APIKeySweeperConfig configures the sweeper.
type APIKeySweeperConfig struct {
	Store    APIKeyExpirer
	Audit    AuditRecorder
	Interval time.Duration
	NowFn    func() time.Time
}

// APIKeySweepInterval returns the configured sweep interval or
// the 5-minute default.
func APIKeySweepInterval() time.Duration {
	if raw := os.Getenv("CONTEXT_ENGINE_API_KEY_SWEEP_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}

// NewAPIKeySweeper validates the config and constructs a sweeper.
func NewAPIKeySweeper(cfg APIKeySweeperConfig) (*APIKeySweeper, error) {
	if cfg.Store == nil {
		return nil, errors.New("api_key_sweeper: Store required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = APIKeySweepInterval()
	}
	if cfg.NowFn == nil {
		cfg.NowFn = func() time.Time { return time.Now().UTC() }
	}
	return &APIKeySweeper{
		store:    cfg.Store,
		audit:    cfg.Audit,
		interval: cfg.Interval,
		nowFn:    cfg.NowFn,
	}, nil
}

// SweepOnce runs a single sweep pass and returns the number of
// rows transitioned.
func (s *APIKeySweeper) SweepOnce(ctx context.Context) (int, error) {
	rows, err := s.store.ExpireGrace(ctx, s.nowFn())
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		observability.APIKeysExpiredTotal.Inc()
		if s.audit != nil {
			log := &audit.AuditLog{
				ID:           ulid.Make().String(),
				TenantID:     r.TenantID,
				ActorID:      "system",
				Action:       audit.ActionAPIKeyExpired,
				ResourceType: "api_key",
				ResourceID:   r.ID,
				CreatedAt:    s.nowFn(),
			}
			if aerr := s.audit.Create(ctx, log); aerr != nil {
				slog.Warn("api_key sweep audit emit failed", "error", aerr, "id", r.ID)
			}
		}
	}
	return len(rows), nil
}

// Run blocks until ctx is canceled, sweeping at the configured
// interval. Calls SweepOnce immediately on entry.
func (s *APIKeySweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	if _, err := s.SweepOnce(ctx); err != nil {
		slog.Warn("api_key sweep initial pass failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.SweepOnce(ctx); err != nil {
				slog.Warn("api_key sweep failed", "error", err)
			}
		}
	}
}
