// backfill_completion.go — Round-5 Task 13.
//
// When a source's sync progress reaches 100% (discovered ==
// processed + failed and at least one row has any movement) the
// system emits a `source.backfill_completed` audit event AND
// pushes a `backfill_completed` SSE message onto every connected
// admin client's stream.
//
// The SSE handler already detects the 100% condition for its
// `completed` event; this file adds the audit-log side-effect plus
// the cross-connection notifier so other admin tabs (or a
// dashboard background poller) see the event even if they are not
// the connection that observed completion.
package admin

import (
	"context"
	"sync"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// BackfillCompletionEmitter records the audit event and fans the
// event out to subscribers. Dedup is in-process: the same (tenant,
// source) pair only emits the audit event once per worker
// lifetime. Restart-survival is intentionally out of scope —
// audit_log is append-only so a sweep on startup can seed the
// in-memory map; cmd/api wires that.
type BackfillCompletionEmitter struct {
	audit AuditWriter

	mu          sync.Mutex
	seen        map[string]bool
	subscribers []chan BackfillCompletedEvent
}

// BackfillCompletedEvent is the payload pushed to subscribers.
type BackfillCompletedEvent struct {
	TenantID string `json:"tenant_id"`
	SourceID string `json:"source_id"`
}

// NewBackfillCompletionEmitter returns an emitter wired to the
// supplied audit writer. The audit writer is required — passing
// nil panics at startup. (The Phase-8 admin wiring already injects
// a non-nil writer; tests use the same fake AuditWriter the rest
// of the suite uses.)
func NewBackfillCompletionEmitter(au AuditWriter) *BackfillCompletionEmitter {
	if au == nil {
		panic("backfill emitter: nil AuditWriter")
	}
	return &BackfillCompletionEmitter{
		audit: au,
		seen:  map[string]bool{},
	}
}

// Subscribe returns a channel that receives every backfill
// completion event observed by this emitter from now on. The
// caller is responsible for draining the channel; full channels
// drop events (the SSE handler treats it as best-effort, like
// heartbeats). Capacity 16 is enough for a burst of completions
// from a multi-source backfill without blocking emit().
func (e *BackfillCompletionEmitter) Subscribe() <-chan BackfillCompletedEvent {
	ch := make(chan BackfillCompletedEvent, 16)
	e.mu.Lock()
	e.subscribers = append(e.subscribers, ch)
	e.mu.Unlock()
	return ch
}

// Unsubscribe removes ch from the subscriber list and closes it.
// Idempotent — calling twice is safe.
//
// The fan-out path (Emit) holds the same `e.mu` while iterating
// over `e.subscribers` and performing non-blocking sends. Because
// the slice mutation and `close(c)` happen here under the lock,
// Emit can never observe a half-removed-but-still-closed channel:
// either the channel is in the slice and open (Emit's send
// proceeds), or it has been removed and closed (Emit doesn't see
// it at all). This eliminates the BUG_0001 race where Emit
// snapshotted under the lock then sent unlocked, allowing an
// interleaved Unsubscribe to close a channel still in the
// snapshot.
func (e *BackfillCompletionEmitter) Unsubscribe(ch <-chan BackfillCompletedEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, c := range e.subscribers {
		if c == ch {
			e.subscribers = append(e.subscribers[:i], e.subscribers[i+1:]...)
			close(c)
			return
		}
	}
}

// MarkSeen records that (tenantID, sourceID) has already been
// emitted. cmd/api calls this once per row when seeding the dedup
// map from the audit_log on startup so a restart doesn't re-emit
// completion events that have already been audited.
func (e *BackfillCompletionEmitter) MarkSeen(tenantID, sourceID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen[backfillKey(tenantID, sourceID)] = true
}

// Emit records the audit event and notifies subscribers. Returns
// true when the event was emitted (first observation), false when
// the (tenant, source) pair was already seen.
//
// Audit-write failures are surfaced as the returned error but the
// in-memory `seen` flag is set regardless — a transient DB error
// shouldn't unleash a flood of duplicate audits as soon as the DB
// recovers. Operators can re-emit the audit row by hand if needed.
func (e *BackfillCompletionEmitter) Emit(ctx context.Context, tenantID, sourceID, actorID string) (bool, error) {
	if tenantID == "" || sourceID == "" {
		return false, nil
	}
	key := backfillKey(tenantID, sourceID)

	// Critical-section discipline (FLAG_pr-review-job_0001 + fix
	// for BUG_pr-review-job-96794a4ddef143afb93842117129455d_0001):
	//
	// Two short critical sections; the blocking audit DB write runs
	// between them with no lock held.
	//
	//   1. First lock: dedup check + flip. Returns early on a
	//      duplicate without touching the DB.
	//   2. Audit write: unlocked. audit.Create can block on
	//      Postgres; running it under the lock would serialize
	//      every completion emission globally and make a slow audit
	//      DB freeze every SSE subscriber.
	//   3. Second lock: non-blocking send to each current
	//      subscriber. We iterate over `e.subscribers` directly
	//      (no snapshot) so a subscriber that called Unsubscribe
	//      before the loop started is already gone from the slice
	//      and we never attempt a send on its closed channel.
	//
	// Why not the previous snapshot-then-send pattern? The previous
	// implementation snapshotted under the lock and ranged over the
	// snapshot unlocked. An Unsubscribe interleaved between the
	// snapshot and a send would close a channel still referenced by
	// the snapshot, and the next `ch <- evt` would panic with
	// "send on closed channel". The select{...default:} guard only
	// protects against a full buffer, not a closed channel.
	//
	// Lock-hold time during fanout is bounded: each iteration is a
	// non-blocking select with a default branch, so total time is
	// O(len(subscribers)) channel-state checks — microseconds even
	// with hundreds of subscribers. Subscribe/Unsubscribe only
	// briefly contend on the slice mutation under the same lock.
	e.mu.Lock()
	if e.seen[key] {
		e.mu.Unlock()
		return false, nil
	}
	e.seen[key] = true
	e.mu.Unlock()

	err := e.audit.Create(ctx, audit.NewAuditLog(
		tenantID, actorID, audit.ActionSourceBackfillCompleted, "source", sourceID,
		audit.JSONMap{"completion_state": "fully_processed"}, "",
	))
	// Notify subscribers regardless of audit success — an SSE client
	// already saw the wire-level `completed` event; the audit row is
	// for the persistent record. Best-effort fanout under the lock
	// so concurrent Unsubscribe (which removes + closes) is
	// serialized against this loop.
	evt := BackfillCompletedEvent{TenantID: tenantID, SourceID: sourceID}
	e.mu.Lock()
	for _, ch := range e.subscribers {
		select {
		case ch <- evt:
		default:
			// Channel full — subscriber is stuck. Drop this event;
			// they'll catch up via the next poll.
		}
	}
	e.mu.Unlock()
	return true, err
}

func backfillKey(tenantID, sourceID string) string {
	return tenantID + "|" + sourceID
}
