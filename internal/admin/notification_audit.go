package admin

// notification_audit.go — Round-8 Task 5.
//
// NotifyingAuditWriter wraps an existing AuditWriter and fires the
// configured NotificationDispatcher every time an audit event is
// persisted. Dispatch failures are intentionally swallowed so they
// can never abort the audit write path.

import (
	"context"
	"sync"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// auditCreator is the narrow contract NotifyingAuditWriter wraps.
// *audit.Repository and tests' fake recorders both satisfy it.
type auditCreator interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// auditTxCreator is the optional extension that admin.NotifyingAuditWriter
// satisfies so policy.AuditWriter (which requires CreateInTx) is happy.
type auditTxCreator interface {
	auditCreator
	CreateInTx(ctx context.Context, tx *gorm.DB, log *audit.AuditLog) error
}

// NotifyingAuditWriter persists an audit event via Inner then arranges
// for the NotificationDispatcher to fire for the (TenantID, Action)
// pair. The non-transactional Create path fires immediately. The
// transactional CreateInTx path buffers the event and only fires
// once the caller invokes Flush after the surrounding tx has
// committed (see the phantom-notification note on CreateInTx below).
//
// The dispatch runs synchronously inside Create/Flush when Async is
// false, or in a fresh background goroutine when Async is true.
// Failures from Dispatch are never returned to the caller — the
// audit write is the source of truth and must not be coupled to
// subscriber health.
type NotifyingAuditWriter struct {
	Inner      auditCreator
	Dispatcher *NotificationDispatcher
	Async      bool

	// Background context used by Async dispatches. Defaults to
	// context.Background. Tests inject their own to cancel pending
	// dispatches deterministically.
	BackgroundCtx context.Context

	// pendingByTx buffers audit logs persisted inside a caller-
	// managed transaction so the notification only fires once the
	// tx has committed. The map is keyed by the *gorm.DB tx handle
	// the caller passed to CreateInTx; the pointer is only used as
	// a per-tx identifier, never to issue further DB ops. Drained
	// by Flush on success or Discard on rollback.
	mu          sync.Mutex
	pendingByTx map[*gorm.DB][]*audit.AuditLog
}

// Create implements AuditWriter. The audit row is always persisted
// first; the dispatch is fired afterwards and its outcome ignored.
//
// Unlike CreateInTx there is no surrounding caller-managed tx to
// commit, so firing the dispatcher here can never produce a
// phantom-notification — by the time we call fire the row is
// already in the database.
func (n *NotifyingAuditWriter) Create(ctx context.Context, log *audit.AuditLog) error {
	if n == nil || n.Inner == nil {
		return audit.ErrAuditLogNotFound // defensive — should never happen in production
	}
	if err := n.Inner.Create(ctx, log); err != nil {
		return err
	}
	n.fire(ctx, log)
	return nil
}

// CreateInTx satisfies policy.AuditWriter (the transactional-outbox
// entry point). The audit row is inserted inside the caller's tx
// and the notification is BUFFERED, not fired. The caller must
// invoke Flush(ctx, tx) after the surrounding tx.Transaction(...)
// returns nil — and Discard(tx) on rollback — so the notification
// only reaches subscribers if the audit row actually committed.
//
// Without this buffering a phantom-notification slips out whenever
// a later step inside the same tx callback fails: the audit row
// rolls back along with the rest of the work, but the webhook /
// email has already been POSTed to external subscribers, who then
// see an event for a state change that never happened. Async mode
// did not save us — Go's scheduler is allowed to run the dispatch
// goroutine immediately, well before the tx commits.
func (n *NotifyingAuditWriter) CreateInTx(ctx context.Context, tx *gorm.DB, log *audit.AuditLog) error {
	if n == nil || n.Inner == nil {
		return audit.ErrAuditLogNotFound
	}
	inner, ok := n.Inner.(auditTxCreator)
	if !ok {
		// Fallback: there's no tx-aware inner. Use Create.
		return n.Create(ctx, log)
	}
	if err := inner.CreateInTx(ctx, tx, log); err != nil {
		return err
	}
	if log == nil || n.Dispatcher == nil {
		// No dispatcher wired or no log to dispatch — nothing to
		// buffer; the inner write is the only side effect.
		return nil
	}
	n.mu.Lock()
	if n.pendingByTx == nil {
		n.pendingByTx = make(map[*gorm.DB][]*audit.AuditLog)
	}
	n.pendingByTx[tx] = append(n.pendingByTx[tx], log)
	n.mu.Unlock()
	return nil
}

// Flush dispatches every audit log that CreateInTx buffered against
// the supplied tx handle and clears the buffer. Callers MUST invoke
// Flush after the surrounding tx.Transaction returns nil so the
// notification only fires after the row has committed.
//
// Calling Flush with an unknown tx (or after Discard) is a no-op.
// Flush is also a no-op when the writer is nil so the policy
// promoter can call it unconditionally.
func (n *NotifyingAuditWriter) Flush(ctx context.Context, tx *gorm.DB) {
	if n == nil {
		return
	}
	n.mu.Lock()
	rows := n.pendingByTx[tx]
	delete(n.pendingByTx, tx)
	n.mu.Unlock()
	for _, log := range rows {
		n.fire(ctx, log)
	}
}

// Discard drops every audit log that CreateInTx buffered against
// the supplied tx handle without firing any notification. Callers
// MUST invoke Discard whenever the surrounding tx.Transaction
// returns a non-nil error so a rolled-back audit row never reaches
// external subscribers.
func (n *NotifyingAuditWriter) Discard(tx *gorm.DB) {
	if n == nil {
		return
	}
	n.mu.Lock()
	delete(n.pendingByTx, tx)
	n.mu.Unlock()
}

// fire dispatches the notification for the freshly-persisted log.
// Errors from Dispatch are swallowed so they can never abort the
// audit write path.
func (n *NotifyingAuditWriter) fire(ctx context.Context, log *audit.AuditLog) {
	if n.Dispatcher == nil || log == nil {
		return
	}
	payload := buildNotificationPayload(log)
	if n.Async {
		bg := n.BackgroundCtx
		if bg == nil {
			bg = context.Background()
		}
		go func() {
			_ = n.Dispatcher.Dispatch(bg, log.TenantID, string(log.Action), payload)
		}()
		return
	}
	_ = n.Dispatcher.Dispatch(ctx, log.TenantID, string(log.Action), payload)
}

// buildNotificationPayload mints the JSON shape published to
// subscribers. We deliberately exclude PII (no full request body)
// and ship the fields operators most often need to dedupe / route:
// id, actor, resource_type, resource_id, trace_id, and metadata.
func buildNotificationPayload(log *audit.AuditLog) map[string]any {
	return map[string]any{
		"id":            log.ID,
		"actor_id":      log.ActorID,
		"resource_type": log.ResourceType,
		"resource_id":   log.ResourceID,
		"trace_id":      log.TraceID,
		"metadata":      map[string]any(log.Metadata),
		"created_at":    log.CreatedAt,
	}
}
