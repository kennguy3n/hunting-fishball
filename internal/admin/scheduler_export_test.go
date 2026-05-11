// Round-12 Task 4 — test-only exposers for scheduler internals.
//
// SafeTickForTest lets tests drive the panic-recovery wrapper
// without exposing safeTick to production callers; the wrapping is
// the API contract under test.
//
// AdvanceNextRunForTest mutates a schedule's next_run_at so a
// follow-up Tick re-picks the row — used to assert the LastError
// reset path after a successful subsequent run.
package admin

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// SafeTickForTest is a thin shim retained for back-compat with
// existing in-package tests; the production wrapper is now
// exported as Scheduler.SafeTick. New callers should use SafeTick.
func (s *Scheduler) SafeTickForTest(ctx context.Context) error {
	return s.SafeTick(ctx)
}

// AdvanceNextRunForTest reaches into the SyncSchedule row and sets
// next_run_at directly. Used by the resilience suite to set up a
// "row becomes due again" scenario.
func AdvanceNextRunForTest(db *gorm.DB, id string, next time.Time) error {
	return db.Model(&SyncSchedule{}).
		Where("id = ?", id).
		Update("next_run_at", next).Error
}
