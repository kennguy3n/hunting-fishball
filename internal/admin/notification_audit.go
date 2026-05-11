package admin

// notification_audit.go — Round-8 Task 5.
//
// NotifyingAuditWriter wraps an existing AuditWriter and fires the
// configured NotificationDispatcher every time an audit event is
// persisted. Dispatch failures are intentionally swallowed so they
// can never abort the audit write path.

import (
	"context"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// auditCreator is the narrow contract NotifyingAuditWriter wraps.
// *audit.Repository and tests' fake recorders both satisfy it. The
// CreateInTx method is optional; when present, NotifyingAuditWriter
// preserves transactional-outbox semantics by deferring the
// notification dispatch until after the caller's transaction has
// committed (Async=true) or by firing immediately (Async=false).
type auditCreator interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// auditTxCreator is the optional extension that admin.NotifyingAuditWriter
// satisfies so policy.AuditWriter (which requires CreateInTx) is happy.
type auditTxCreator interface {
	auditCreator
	CreateInTx(ctx context.Context, tx *gorm.DB, log *audit.AuditLog) error
}

// NotifyingAuditWriter persists an audit event via Inner then fires
// the NotificationDispatcher for the (TenantID, Action) pair. The
// dispatch runs synchronously inside Create when Async is false, or
// in a fresh background goroutine when Async is true. Failures from
// Dispatch are never returned to the caller — the audit write is the
// source of truth and must not be coupled to subscriber health.
type NotifyingAuditWriter struct {
	Inner      auditCreator
	Dispatcher *NotificationDispatcher
	Async      bool

	// Background context used by Async dispatches. Defaults to
	// context.Background. Tests inject their own to cancel pending
	// dispatches deterministically.
	BackgroundCtx context.Context
}

// Create implements AuditWriter. The audit row is always persisted
// first; the dispatch is fired afterwards and its outcome ignored.
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
// entry point). The dispatcher is only fired when the inner repo
// successfully persists the row; production paths run this inside a
// caller-managed tx so the audit row commits atomically with the
// business write. Dispatch fires after CreateInTx returns — in
// Async mode the goroutine fires once the surrounding tx has been
// committed and the caller has returned.
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
	n.fire(ctx, log)
	return nil
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
