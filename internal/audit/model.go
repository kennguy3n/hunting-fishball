// Package audit implements the audit-log primitives for hunting-fishball:
//
//   - A GORM model + migration for the audit_logs table
//   - A repository that writes new entries inside the caller's transaction
//     (transactional outbox)
//   - A background poller that publishes unpublished entries to Kafka
//   - A Gin HTTP handler that serves /v1/audit-logs to the admin portal
//
// The shape mirrors uneycom/ai-agent-platform's audit log: append-only,
// ULID-keyed (lexicographically sortable, IDs sort by time), tenant-scoped
// at the index level. Reads always start with `account_id = ?` so the row
// can never escape its tenant.
package audit

import (
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
)

// Action enumerates the audit categories the platform emits. Keep this
// list narrow — it's the set the admin portal uses to populate the
// filter dropdown.
type Action string

const (
	ActionConnectorRegistered      Action = "connector.registered"
	ActionConnectorConnected       Action = "connector.connected"
	ActionSourceConnected          Action = "source.connected"
	ActionSourcePaused             Action = "source.paused"
	ActionSourceResumed            Action = "source.resumed"
	ActionSourceReScoped           Action = "source.re_scoped"
	ActionSourceCredentialsRotated Action = "source.credentials_rotated"
	ActionSourceSyncStarted        Action = "source.sync_started"
	ActionChunkIndexed             Action = "chunk.indexed"
	ActionChunkExpired             Action = "chunk.expired"
	ActionSourceSynced             Action = "source.synced"
	ActionSourcePurged             Action = "source.purged"
	ActionRetrievalQueried         Action = "retrieval.queried"
	ActionPolicyEdited             Action = "policy.edited"
	ActionPolicyApplied            Action = "policy.applied"
	ActionPolicyDrafted            Action = "policy.drafted"
	ActionPolicyPromoted           Action = "policy.promoted"
	ActionPolicyRejected           Action = "policy.rejected"
	ActionAuditRead                Action = "audit.read"
	ActionDLQReplayed              Action = "dlq.replayed"
	ActionTenantDeletionRequested  Action = "tenant.deletion_requested"
	ActionTenantDeleted            Action = "tenant.deleted"
	ActionReindexRequested         Action = "reindex.requested"
	// Round-5 admin lifecycle additions.
	ActionSourceTokenRefreshed     Action = "source.token_refreshed"
	ActionSourceCredentialExpiring Action = "source.credential_expiring"
	ActionSourceCredentialExpired  Action = "source.credential_expired"
	ActionSourceBackfillCompleted  Action = "source.backfill_completed"
	ActionWebhookReceived          Action = "webhook.received"
	ActionWebhookProcessed         Action = "webhook.processed"
	ActionWebhookFailed            Action = "webhook.failed"
	ActionRetrievalFeedback        Action = "retrieval.feedback"
	ActionPolicyRolledBack         Action = "policy.rolled_back"
	ActionTenantExportRequested    Action = "tenant.export_requested"
	ActionTenantExportCompleted    Action = "tenant.export_completed"
	ActionIndexAutoReindex         Action = "index.auto_reindex_triggered"
	ActionConnectorPreviewed       Action = "connector.previewed"
)

// AuditLog is a single audit event. The table is append-only — there is
// no UpdatedAt or DeletedAt. Sorting "newest first" is implemented as
// ORDER BY id DESC because IDs are time-ordered ULIDs.
type AuditLog struct {
	// ID is a 26-char ULID. We store it as text rather than UUID so it
	// remains sortable in PG without an extra btree on created_at.
	ID string `gorm:"type:char(26);primaryKey;column:id" json:"id"`

	// TenantID is the multi-tenant scope. Every read query must include
	// `tenant_id = ?` — enforced at the handler layer. Indexes that
	// include this column live in migrations/001_audit_log.sql.
	TenantID string `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`

	// ActorID is the ULID of the user / member / service account that
	// caused the event. May be empty for system-generated events.
	ActorID string `gorm:"type:char(26);column:actor_id" json:"actor_id,omitempty"`

	// Action names the event ("source.synced", "retrieval.queried", ...).
	Action Action `gorm:"type:varchar(64);not null;column:action" json:"action"`

	// ResourceType classifies the target ("source", "policy",
	// "retrieval-call").
	ResourceType string `gorm:"type:varchar(64);not null;column:resource_type" json:"resource_type"`

	// ResourceID is the ULID of the target resource.
	ResourceID string `gorm:"type:char(26);column:resource_id" json:"resource_id,omitempty"`

	// Metadata is connector- or action-specific extras (request bytes,
	// response counts, ...). JSONB on disk; serialized as a JSON object
	// over the wire.
	Metadata JSONMap `gorm:"type:jsonb;not null;default:'{}';column:metadata" json:"metadata"`

	// TraceID, when present, ties the audit row to the OpenTelemetry
	// trace that produced it. Indexed so the on-call can pivot from a
	// log row to the full trace.
	TraceID string `gorm:"type:varchar(64);column:trace_id" json:"trace_id,omitempty"`

	// CreatedAt is the wall-clock time the row was inserted. The ULID
	// embeds a timestamp too, but having a real column simplifies the
	// retention sweep query.
	CreatedAt time.Time `gorm:"not null;default:now();column:created_at" json:"created_at"`

	// PublishedAt is set by the outbox poller after the row has been
	// published to Kafka. Nil means "still pending publish".
	PublishedAt *time.Time `gorm:"column:published_at" json:"-"`
}

// Indexes for this table are defined in migrations/001_audit_log.sql,
// which is the single source of truth for the audit_logs schema.
// The model deliberately omits GORM `index:` tags so AutoMigrate cannot
// drift from the migration.

// TableName overrides the default GORM pluralization.
func (AuditLog) TableName() string { return "audit_logs" }

// NewAuditLog constructs a new entry with a fresh ULID and the supplied
// fields. Metadata is shallow-copied so callers can mutate the source map
// without aliasing the persisted row.
func NewAuditLog(
	tenantID, actorID string,
	action Action,
	resourceType, resourceID string,
	metadata JSONMap,
	traceID string,
) *AuditLog {
	cp := JSONMap{}
	for k, v := range metadata {
		cp[k] = v
	}

	return &AuditLog{
		ID:           ulid.Make().String(),
		TenantID:     tenantID,
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Metadata:     cp,
		TraceID:      traceID,
		CreatedAt:    time.Now().UTC(),
	}
}

// ErrInvalidAuditLog is returned by Validate when a constructed AuditLog
// is missing required fields.
var ErrInvalidAuditLog = errors.New("audit: invalid audit log")

// Validate checks the invariants the table column constraints would
// otherwise reject at insert time. Used by the repository before opening
// a transaction.
func (a *AuditLog) Validate() error {
	if a.ID == "" {
		return errors.New("audit: missing ID")
	}
	if a.TenantID == "" {
		return errors.New("audit: missing tenant_id")
	}
	if a.Action == "" {
		return errors.New("audit: missing action")
	}
	if a.ResourceType == "" {
		return errors.New("audit: missing resource_type")
	}

	return nil
}
