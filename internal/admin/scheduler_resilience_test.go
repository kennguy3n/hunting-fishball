// Round-12 Task 4 — scheduler panic recovery + last_error
// persistence.
//
// Asserts:
//  1. safeTick swallows a panic inside the emitter chain and
//     converts it to a slog warn + counter bump.
//  2. A returned (non-panic) emit error persists last_error /
//     last_error_at on the sync_schedules row.
//  3. A successful subsequent tick clears the error fields.
package admin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

type panickyEmitter struct{ panics int }

func (p *panickyEmitter) EmitSync(_ context.Context, _, _ string) error {
	p.panics++
	panic("simulated emitter panic")
}

type erroringEmitter struct{ err error }

func (e *erroringEmitter) EmitSync(_ context.Context, _, _ string) error {
	return e.err
}

func TestScheduler_PanicRecovery_DoesNotKillGoroutine(t *testing.T) {
	db := newSchedulerDB(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := admin.UpsertSchedule(context.Background(), db, "tenant-a", "src-a", "@every 5m", true, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	emit := &panickyEmitter{}
	s, err := admin.NewScheduler(admin.SchedulerConfig{
		DB: db, Emitter: emit,
		Now: func() time.Time { return now.Add(10 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	before := testutil.ToFloat64(observability.SchedulerErrorsTotal)

	// SafeTick must convert the panic to an error so callers see a
	// clean return path.
	if err := s.SafeTickForTest(context.Background()); err == nil {
		t.Fatalf("safeTick must surface panic as error")
	}
	if emit.panics != 1 {
		t.Fatalf("expected emitter called once before panic, got %d", emit.panics)
	}
	got := testutil.ToFloat64(observability.SchedulerErrorsTotal) - before
	if got < 1 {
		t.Fatalf("scheduler errors counter expected to increment on panic, got delta=%v", got)
	}
}

func TestScheduler_PersistsLastErrorOnEmitFailure(t *testing.T) {
	db := newSchedulerDB(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := admin.UpsertSchedule(context.Background(), db, "tenant-a", "src-a", "@every 5m", true, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	wantErr := errors.New("kafka timeout: producer disconnected")
	s, err := admin.NewScheduler(admin.SchedulerConfig{
		DB: db, Emitter: &erroringEmitter{err: wantErr},
		Now: func() time.Time { return now.Add(10 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick must not return: %v", err)
	}
	row, err := admin.GetSchedule(context.Background(), db, "tenant-a", "src-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.LastError == "" {
		t.Fatalf("LastError empty after emit failure")
	}
	if row.LastErrorAt.IsZero() {
		t.Fatalf("LastErrorAt zero after emit failure")
	}
}

func TestScheduler_ClearsLastErrorOnSuccess(t *testing.T) {
	db := newSchedulerDB(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := admin.UpsertSchedule(context.Background(), db, "tenant-a", "src-a", "@every 1m", true, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// First tick — emitter fails, last_error is set.
	failing := &erroringEmitter{err: errors.New("transient kafka error")}
	t1 := now.Add(2 * time.Minute)
	s1, _ := admin.NewScheduler(admin.SchedulerConfig{
		DB: db, Emitter: failing,
		Now: func() time.Time { return t1 },
	})
	if err := s1.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	row, _ := admin.GetSchedule(context.Background(), db, "tenant-a", "src-a")
	if row.LastError == "" {
		t.Fatalf("expected LastError populated after failure")
	}

	// Bump next_run_at so the same row becomes due again, then run
	// a successful tick.
	if err := admin.AdvanceNextRunForTest(db, row.ID, t1.Add(-time.Minute)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	ok := &recordingEmitter{}
	s2, _ := admin.NewScheduler(admin.SchedulerConfig{
		DB: db, Emitter: ok,
		Now: func() time.Time { return t1.Add(5 * time.Minute) },
	})
	if err := s2.Tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	row2, _ := admin.GetSchedule(context.Background(), db, "tenant-a", "src-a")
	if row2.LastError != "" {
		t.Fatalf("expected LastError cleared after success, got %q", row2.LastError)
	}
}
