package admin_test

// source_auto_pause_test.go — Round-19 Task 24.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type fakeAutoPausePauser struct {
	mu     sync.Mutex
	called int
	args   []struct{ Tenant, Source string }
	err    error
}

func (f *fakeAutoPausePauser) PauseSource(_ context.Context, tenantID, sourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.args = append(f.args, struct{ Tenant, Source string }{tenantID, sourceID})

	return f.err
}

type fakeAutoPauseAuditWriter struct {
	mu     sync.Mutex
	called int
	last   *audit.AuditLog
}

func (f *fakeAutoPauseAuditWriter) Create(_ context.Context, log *audit.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.last = log

	return nil
}

// flakyAutoPauseAuditWriter fails its first Create call when
// failNext is set, then succeeds. Used to exercise the rollback
// branch when audit persistence fails after PauseSource succeeds.
type flakyAutoPauseAuditWriter struct {
	mu       sync.Mutex
	called   int
	failNext bool
}

func (f *flakyAutoPauseAuditWriter) Create(_ context.Context, _ *audit.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	if f.failNext {
		f.failNext = false

		return errors.New("transient audit failure")
	}

	return nil
}

func TestSourceAutoPauser_DoesNotPauseBelowThreshold(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{}
	writer := &fakeAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:     time.Minute,
		WindowBuckets:      4,
		ErrorRateThreshold: 0.5,
		MinSampleSize:      4,
		Now:                func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// 1 error in 4 samples — well under threshold.
	for i := 0; i < 3; i++ {
		if err := p.Record(context.Background(), "t", "s", true); err != nil {
			t.Fatalf("Record ok: %v", err)
		}
	}
	if err := p.Record(context.Background(), "t", "s", false); err != nil {
		t.Fatalf("Record err: %v", err)
	}
	if pauser.called != 0 {
		t.Fatalf("pauser must not be called under threshold, got %d", pauser.called)
	}
	if writer.called != 0 {
		t.Fatalf("writer must not be called under threshold, got %d", writer.called)
	}
}

func TestSourceAutoPauser_PausesAtThreshold(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{}
	writer := &fakeAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:     time.Minute,
		WindowBuckets:      4,
		ErrorRateThreshold: 0.5,
		MinSampleSize:      4,
		Now:                func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// 3 errors in 4 samples — > 0.5.
	for i := 0; i < 3; i++ {
		if err := p.Record(context.Background(), "t", "s", false); err != nil {
			t.Fatalf("Record err: %v", err)
		}
	}
	if err := p.Record(context.Background(), "t", "s", true); err != nil {
		t.Fatalf("Record ok: %v", err)
	}
	if pauser.called != 1 {
		t.Fatalf("pauser must be called exactly once on first breach, got %d", pauser.called)
	}
	if writer.called != 1 {
		t.Fatalf("writer must be called exactly once on first breach, got %d", writer.called)
	}
	if writer.last == nil || writer.last.Action != audit.ActionSourceAutoPaused {
		t.Fatalf("audit log must use ActionSourceAutoPaused, got %+v", writer.last)
	}
}

func TestSourceAutoPauser_CooldownPreventsRepeatedPauses(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{}
	writer := &fakeAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:     time.Minute,
		WindowBuckets:      4,
		ErrorRateThreshold: 0.5,
		MinSampleSize:      4,
		PauseCooldown:      time.Hour,
		Now:                func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for i := 0; i < 4; i++ {
		_ = p.Record(context.Background(), "t", "s", false)
	}
	if pauser.called != 1 {
		t.Fatalf("first breach should pause once, got %d", pauser.called)
	}
	// Further errors must not trigger another pause within cooldown.
	for i := 0; i < 4; i++ {
		_ = p.Record(context.Background(), "t", "s", false)
	}
	if pauser.called != 1 {
		t.Fatalf("cooldown must suppress second pause, got %d", pauser.called)
	}
}

func TestSourceAutoPauser_RetriesWhenPauseSourceFails(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{err: errors.New("transient pause failure")}
	writer := &fakeAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:     time.Minute,
		WindowBuckets:      4,
		ErrorRateThreshold: 0.5,
		MinSampleSize:      4,
		PauseCooldown:      time.Hour,
		Now:                func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// First breach: PauseSource fails. We expect Record to surface
	// the error AND to leave lastPaused unset so the next breach
	// can retry — i.e. a failed pause must not consume the cooldown.
	for i := 0; i < 4; i++ {
		_ = p.Record(context.Background(), "t", "s", false)
	}
	if pauser.called != 1 {
		t.Fatalf("first breach should attempt pause once, got %d", pauser.called)
	}
	if writer.called != 0 {
		t.Fatalf("audit must not be written when PauseSource fails, got %d", writer.called)
	}
	// Second breach within cooldown should retry now that the
	// failure is resolved.
	pauser.mu.Lock()
	pauser.err = nil
	pauser.mu.Unlock()
	for i := 0; i < 4; i++ {
		_ = p.Record(context.Background(), "t", "s", false)
	}
	if pauser.called < 2 {
		t.Fatalf("pause must be retried after a failed attempt, got %d", pauser.called)
	}
	if writer.called != 1 {
		t.Fatalf("audit must be written exactly once on successful retry, got %d", writer.called)
	}
}

func TestSourceAutoPauser_RetriesWhenAuditWriterFails(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{}
	writer := &flakyAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:     time.Minute,
		WindowBuckets:      4,
		ErrorRateThreshold: 0.5,
		MinSampleSize:      4,
		PauseCooldown:      time.Hour,
		Now:                func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// First breach: PauseSource succeeds but the audit write
	// fails. Without the pendingPause rollback, lastPaused would
	// be stamped and the next breach would be suppressed for the
	// full cooldown window. With the fix, the second breach
	// retries the entire pause+audit pair.
	writer.failNext = true
	for i := 0; i < 4; i++ {
		_ = p.Record(context.Background(), "t", "s", false)
	}
	if pauser.called != 1 {
		t.Fatalf("first breach should pause once even if audit fails, got %d", pauser.called)
	}
	if writer.called != 1 {
		t.Fatalf("audit must be attempted once on first breach, got %d", writer.called)
	}
	for i := 0; i < 4; i++ {
		_ = p.Record(context.Background(), "t", "s", false)
	}
	if pauser.called < 2 || writer.called < 2 {
		t.Fatalf("audit-write failure must not consume cooldown: pauser=%d writer=%d", pauser.called, writer.called)
	}
}

func TestSourceAutoPauser_RejectsMissingArgs(t *testing.T) {
	t.Parallel()
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{}, &fakeAutoPausePauser{}, &fakeAutoPauseAuditWriter{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := p.Record(context.Background(), "", "s", true); err == nil {
		t.Fatal("expected error on empty tenant")
	}
	if err := p.Record(context.Background(), "t", "", true); err == nil {
		t.Fatal("expected error on empty source")
	}
}

// TestSourceAutoPauser_PausesOnConsecutiveFailures — Round-24 Task 12.
//
// Asserts the consecutive-failure trigger fires before the
// sliding-window rate ever has a chance: 3 contiguous errors with
// threshold=3 trips the pause even though MinSampleSize=20 is far
// above the observed total.
func TestSourceAutoPauser_PausesOnConsecutiveFailures(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{}
	writer := &fakeAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:              time.Minute,
		WindowBuckets:               4,
		ErrorRateThreshold:          0.99,
		MinSampleSize:               20,
		ConsecutiveFailureThreshold: 3,
		Now:                         func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Record(context.Background(), "t", "s", false); err != nil {
			t.Fatalf("Record err: %v", err)
		}
	}
	if pauser.called != 1 {
		t.Fatalf("pauser must be called once on 3rd consecutive failure, got %d", pauser.called)
	}
	if writer.called != 1 {
		t.Fatalf("writer must be called once, got %d", writer.called)
	}
	meta := writer.last.Metadata
	if got, _ := meta["trigger"].(string); got != "consecutive_failures" {
		t.Fatalf("audit trigger=%q, want consecutive_failures (%+v)", got, meta)
	}
}

// TestSourceAutoPauser_ConsecutiveCounterResetsOnSuccess — a
// success between failures must restart the run-length, so two
// errors followed by a success followed by two more errors does
// NOT trip the threshold=3 consecutive trigger.
func TestSourceAutoPauser_ConsecutiveCounterResetsOnSuccess(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{}
	writer := &fakeAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:              time.Minute,
		WindowBuckets:               4,
		ErrorRateThreshold:          0.99,
		MinSampleSize:               20,
		ConsecutiveFailureThreshold: 3,
		Now:                         func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for _, ok := range []bool{false, false, true, false, false} {
		if err := p.Record(context.Background(), "t", "s", ok); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if pauser.called != 0 {
		t.Fatalf("pauser must not fire when success resets the run, got %d", pauser.called)
	}
}

// TestSourceAutoPauser_ConcurrentRecord_NoDataRace exercises the
// pauser from many goroutines simultaneously. Under `go test -race`
// this would surface any read of state.consecutiveFailures (or any
// other state field) that happens outside the protecting mutex. It
// guards the snapshot-under-lock fix for the consecutive_failures
// audit metadata.
func TestSourceAutoPauser_ConcurrentRecord_NoDataRace(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	pauser := &fakeAutoPausePauser{}
	writer := &fakeAutoPauseAuditWriter{}
	p, err := admin.NewSourceAutoPauser(admin.SourceAutoPauseConfig{
		WindowDuration:              time.Minute,
		WindowBuckets:               4,
		ErrorRateThreshold:          0.5,
		MinSampleSize:               4,
		ConsecutiveFailureThreshold: 3,
		Now:                         func() time.Time { return now },
	}, pauser, writer)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				ok := (i+j)%3 == 0 // mix of successes + failures
				if err := p.Record(context.Background(), "t", "s", ok); err != nil {
					t.Errorf("Record: %v", err)

					return
				}
			}
		}()
	}
	wg.Wait()
}
