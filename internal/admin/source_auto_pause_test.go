package admin_test

// source_auto_pause_test.go — Round-19 Task 24.

import (
	"context"
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
