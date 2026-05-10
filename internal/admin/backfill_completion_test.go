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

// TestBackfillCompletion_ConcurrentEmitAndUnsubscribe is the
// regression test for BUG_pr-review-job-96794a4ddef143afb93842117129455d_0001:
// the previous Emit implementation snapshotted e.subscribers under
// the lock and then ranged over the snapshot unlocked. An
// interleaved Unsubscribe could close a channel that was still in
// the snapshot, causing the next `ch <- evt` to panic with
// "send on closed channel".
//
// This test races many goroutines:
//   - 8 subscriber goroutines that repeatedly Subscribe, drain a
//     few events, and Unsubscribe (the unsubscribe closes the
//     channel under the lock).
//   - 4 emitter goroutines that call Emit on a rotating set of
//     (tenant, source) pairs so the dedup map doesn't suppress
//     every call.
//
// Run under `-race`: any send on a closed channel will panic and
// fail the test; the race detector also flags any unsynchronized
// access between Emit's send loop and Unsubscribe's close.
func TestBackfillCompletion_ConcurrentEmitAndUnsubscribe(t *testing.T) {
	t.Parallel()
	au := &auditCapture{}
	em := admin.NewBackfillCompletionEmitter(au)

	const (
		subscribers = 8
		emitters    = 4
		emissions   = 5000
	)

	stop := make(chan struct{})
	var subWg, emWg sync.WaitGroup

	// Subscriber churn: keep running until `stop` is closed.
	for i := 0; i < subscribers; i++ {
		subWg.Add(1)
		go func() {
			defer subWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ch := em.Subscribe()
				// Drain a few events non-blocking, then unsubscribe.
				// The non-blocking drain keeps each goroutine cycling
				// quickly through Subscribe/Unsubscribe, which is
				// where the race manifests.
				for j := 0; j < 4; j++ {
					select {
					case <-ch:
					default:
					}
				}
				em.Unsubscribe(ch)
			}
		}()
	}

	// Emitters: each goroutine cycles through a unique (tenant,
	// source) pair so the dedup map doesn't make every call a
	// no-op. With the buggy implementation, one of these sends
	// would panic on a closed channel before the loop finishes.
	for e := 0; e < emitters; e++ {
		emWg.Add(1)
		go func(eid int) {
			defer emWg.Done()
			for k := 0; k < emissions; k++ {
				tenant := "tenant-x"
				src := "src-" + string(rune('a'+eid)) + "-" + itoa(k)
				_, _ = em.Emit(context.Background(), tenant, src, "actor")
			}
		}(e)
	}

	// Wait for all emitters to finish, then stop subscriber churn.
	emWg.Wait()
	close(stop)
	subWg.Wait()
}

// itoa is a tiny strconv-free integer formatter used by the
// concurrency test to keep imports minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
