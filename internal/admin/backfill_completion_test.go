package admin_test

import (
	"context"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// auditCapture is a small AuditWriter that records every write so
// the backfill completion tests can assert what landed on the
// log. Mirrors the recordAudit pattern used by the DLQ handler
// tests but lives in this file so the two suites compile
// independently.
type auditCapture struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (a *auditCapture) Create(_ context.Context, l *audit.AuditLog) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, l)
	return nil
}

func (a *auditCapture) Calls() []*audit.AuditLog {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*audit.AuditLog, len(a.logs))
	copy(out, a.logs)
	return out
}

func TestBackfillCompletion_FirstEmitWritesAudit(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)
	emitted, err := em.Emit(context.Background(), "tenant-a", "src-1", "actor-a")
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !emitted {
		t.Fatalf("expected first emit to return true")
	}
	calls := au.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 audit call, got %d", len(calls))
	}
	if calls[0].Action != audit.ActionSourceBackfillCompleted {
		t.Fatalf("audit action: %q", calls[0].Action)
	}
	if calls[0].ResourceID != "src-1" || calls[0].TenantID != "tenant-a" {
		t.Fatalf("target/tenant mismatch: %+v", calls[0])
	}
}

func TestBackfillCompletion_DedupSecondEmitNoOp(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)
	if _, err := em.Emit(context.Background(), "tenant-a", "src-1", ""); err != nil {
		t.Fatal(err)
	}
	emitted, err := em.Emit(context.Background(), "tenant-a", "src-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if emitted {
		t.Fatalf("expected dedup on second emit")
	}
	if len(au.Calls()) != 1 {
		t.Fatalf("expected 1 audit call after dedup, got %d", len(au.Calls()))
	}
}

func TestBackfillCompletion_SubscribeReceivesEvent(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)
	ch := em.Subscribe()
	if _, err := em.Emit(context.Background(), "tenant-a", "src-9", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case evt := <-ch:
		if evt.TenantID != "tenant-a" || evt.SourceID != "src-9" {
			t.Fatalf("evt: %+v", evt)
		}
	default:
		t.Fatalf("expected event on subscriber channel")
	}
}

func TestBackfillCompletion_MultipleSubscribersAllReceive(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)
	ch1 := em.Subscribe()
	ch2 := em.Subscribe()
	if _, err := em.Emit(context.Background(), "tenant-a", "src-7", ""); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan admin.BackfillCompletedEvent{ch1, ch2} {
		select {
		case evt := <-ch:
			if evt.SourceID != "src-7" {
				t.Fatalf("unexpected source: %+v", evt)
			}
		default:
			t.Fatalf("expected each subscriber to receive event")
		}
	}
}

func TestBackfillCompletion_UnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)
	ch := em.Subscribe()
	em.Unsubscribe(ch)
	if _, err := em.Emit(context.Background(), "tenant-a", "src-2", ""); err != nil {
		t.Fatal(err)
	}
	// channel should be closed; reading must not block.
	if _, ok := <-ch; ok {
		t.Fatalf("expected closed channel after Unsubscribe")
	}
}

func TestBackfillCompletion_MarkSeenSkipsAudit(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)
	em.MarkSeen("tenant-a", "src-3")
	emitted, err := em.Emit(context.Background(), "tenant-a", "src-3", "")
	if err != nil {
		t.Fatal(err)
	}
	if emitted {
		t.Fatalf("expected pre-marked-seen pair to skip emit")
	}
	if len(au.Calls()) != 0 {
		t.Fatalf("expected no audit writes when seen, got %d", len(au.Calls()))
	}
}

func TestBackfillCompletion_TenantSourceScoping(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)
	// tenant-a/src-1 emits.
	if _, err := em.Emit(context.Background(), "tenant-a", "src-1", ""); err != nil {
		t.Fatal(err)
	}
	// tenant-b/src-1 should still emit (different tenant).
	emitted, err := em.Emit(context.Background(), "tenant-b", "src-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !emitted {
		t.Fatalf("expected cross-tenant duplicate to still emit")
	}
	if len(au.Calls()) != 2 {
		t.Fatalf("expected 2 audit calls (per-tenant), got %d", len(au.Calls()))
	}
}

func TestBackfillCompletion_NilEmitter_NoPanic(t *testing.T) {
	t.Parallel()
	// Constructing with nil audit writer must panic — guards against
	// a wiring mistake at startup.
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil AuditWriter")
		}
	}()
	_ = admin.NewBackfillCompletionEmitter(nil)
}
