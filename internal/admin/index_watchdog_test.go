package admin_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type fakeChecker struct {
	name string
	err  error
}

func (f *fakeChecker) Name() string                { return f.name }
func (f *fakeChecker) Check(context.Context) error { return f.err }

type fakeReindexer struct {
	mu    sync.Mutex
	calls []pipeline.ReindexRequest
}

func (f *fakeReindexer) Reindex(_ context.Context, req pipeline.ReindexRequest) (pipeline.ReindexResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return pipeline.ReindexResult{EventsEmitted: 1}, nil
}

func (f *fakeReindexer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestIndexWatchdog_NoTriggerOnHealthy(t *testing.T) {
	t.Parallel()
	ri := &fakeReindexer{}
	w, err := admin.NewIndexWatchdog(admin.WatchdogConfig{
		Checkers:  []admin.BackendChecker{&fakeChecker{name: "qdrant"}},
		Lister:    &fakeMonitorLister{srcs: []admin.Source{{ID: "src", TenantID: "t"}}},
		Reindexer: ri,
	})
	if err != nil {
		t.Fatalf("NewIndexWatchdog: %v", err)
	}
	w.Tick(context.Background())
	if ri.callCount() != 0 {
		t.Fatalf("healthy backend should not trigger reindex, got %d", ri.callCount())
	}
}

func TestIndexWatchdog_TriggersAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()
	ck := &fakeChecker{name: "qdrant", err: errors.New("timeout")}
	au := &auditCapture{}
	ri := &fakeReindexer{}
	w, _ := admin.NewIndexWatchdog(admin.WatchdogConfig{
		Checkers:  []admin.BackendChecker{ck},
		Lister:    &fakeMonitorLister{srcs: []admin.Source{{ID: "src-1", TenantID: "tenant-a", Status: admin.SourceStatusActive}}},
		Reindexer: ri,
		Audit:     au,
	})
	// First tick: 1 consecutive failure — below threshold.
	w.Tick(context.Background())
	if ri.callCount() != 0 {
		t.Fatalf("tick 1: should not trigger yet, got %d", ri.callCount())
	}
	// Second tick: 2 consecutive failures — meets threshold.
	w.Tick(context.Background())
	if ri.callCount() != 1 {
		t.Fatalf("tick 2: expected reindex trigger, got %d", ri.callCount())
	}
	calls := au.Calls()
	if len(calls) != 1 || calls[0].Action != audit.ActionIndexAutoReindex {
		t.Fatalf("audit: %+v", calls)
	}
}

func TestIndexWatchdog_ResetsOnRecovery(t *testing.T) {
	t.Parallel()
	ck := &fakeChecker{name: "qdrant", err: errors.New("fail")}
	ri := &fakeReindexer{}
	w, _ := admin.NewIndexWatchdog(admin.WatchdogConfig{
		Checkers:  []admin.BackendChecker{ck},
		Lister:    &fakeMonitorLister{srcs: []admin.Source{{ID: "s", TenantID: "t"}}},
		Reindexer: ri,
	})
	w.Tick(context.Background())
	// Backend recovers.
	ck.err = nil
	w.Tick(context.Background())
	f := w.ConsecutiveFailures()
	if f["qdrant"] != 0 {
		t.Fatalf("consecutive failures not reset: %d", f["qdrant"])
	}
}

func TestIndexWatchdog_CooldownPreventsFlood(t *testing.T) {
	t.Parallel()
	ck := &fakeChecker{name: "qdrant", err: errors.New("fail")}
	ri := &fakeReindexer{}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w, _ := admin.NewIndexWatchdog(admin.WatchdogConfig{
		Checkers:  []admin.BackendChecker{ck},
		Lister:    &fakeMonitorLister{srcs: []admin.Source{{ID: "s", TenantID: "t"}}},
		Reindexer: ri,
		Now:       func() time.Time { return clock },
	})
	// Two ticks to reach threshold.
	w.Tick(context.Background())
	w.Tick(context.Background())
	if ri.callCount() != 1 {
		t.Fatalf("first trigger: %d", ri.callCount())
	}
	// Third tick at same time should be rate-limited.
	w.Tick(context.Background())
	if ri.callCount() != 1 {
		t.Fatalf("cooldown not enforced: %d", ri.callCount())
	}
	// Advance past cooldown.
	clock = clock.Add(admin.WatchdogReindexCooldown + time.Second)
	w.Tick(context.Background())
	if ri.callCount() != 2 {
		t.Fatalf("after cooldown should trigger again: %d", ri.callCount())
	}
}

func TestIndexWatchdog_MultiTenantReindex(t *testing.T) {
	t.Parallel()
	ck := &fakeChecker{name: "qdrant", err: errors.New("fail")}
	ri := &fakeReindexer{}
	w, _ := admin.NewIndexWatchdog(admin.WatchdogConfig{
		Checkers: []admin.BackendChecker{ck},
		Lister: &fakeMonitorLister{srcs: []admin.Source{
			{ID: "s1", TenantID: "tenant-a"},
			{ID: "s2", TenantID: "tenant-a"},
			{ID: "s3", TenantID: "tenant-b"},
		}},
		Reindexer: ri,
	})
	w.Tick(context.Background())
	w.Tick(context.Background())
	// All 3 source×tenant pairs trigger.
	if ri.callCount() != 3 {
		t.Fatalf("expected 3 reindex calls, got %d", ri.callCount())
	}
}

func TestIndexWatchdog_NilListerErrors(t *testing.T) {
	t.Parallel()
	_, err := admin.NewIndexWatchdog(admin.WatchdogConfig{
		Checkers:  []admin.BackendChecker{&fakeChecker{name: "q"}},
		Reindexer: &fakeReindexer{},
	})
	if err == nil {
		t.Fatal("expected error for nil Lister")
	}
}

func TestIndexWatchdog_NilReindexerErrors(t *testing.T) {
	t.Parallel()
	_, err := admin.NewIndexWatchdog(admin.WatchdogConfig{
		Checkers: []admin.BackendChecker{&fakeChecker{name: "q"}},
		Lister:   &fakeMonitorLister{},
	})
	if err == nil {
		t.Fatal("expected error for nil Reindexer")
	}
}
