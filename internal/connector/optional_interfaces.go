package connector

import "context"

// DeltaSyncer is implemented by connectors that can return a delta-encoded
// list of changes given an opaque cursor. This is the cheap path between
// "full rescan" and "live subscription"; pollers call DeltaSync on a
// schedule and persist the new cursor.
//
// A connector advertises the capability by also implementing this
// interface. Callers detect support via:
//
//	if ds, ok := c.(DeltaSyncer); ok { ... }
type DeltaSyncer interface {
	// DeltaSync returns the changes that occurred since the last call,
	// keyed off the opaque cursor. The new cursor must be persisted so
	// the next call sees the same events at most once.
	//
	// An empty cursor on the first call is acceptable; the connector
	// returns the current cursor without backfilling history. Callers
	// that want a backfill should use ListDocuments instead.
	DeltaSync(ctx context.Context, conn Connection, ns Namespace, cursor string) ([]DocumentChange, string, error)
}

// WebhookReceiver is implemented by connectors that accept upstream change
// notifications over HTTP. The platform backend mounts WebhookPath under
// /v1/connectors/{name}/webhook and calls HandleWebhook with the raw
// payload bytes; the connector returns the decoded changes.
type WebhookReceiver interface {
	// HandleWebhook decodes a raw webhook payload (already verified for
	// signature by the connector or the caller) into a list of changes.
	HandleWebhook(ctx context.Context, payload []byte) ([]DocumentChange, error)

	// WebhookPath is the path suffix the platform mounts the receiver at.
	// Should include a leading slash, e.g. "/notion".
	WebhookPath() string
}

// WebhookVerifier is implemented by WebhookReceivers that want the
// platform to validate the request signature before HandleWebhook is
// invoked. The map[string][]string mirrors http.Header so the
// interface stays decoupled from net/http while preserving the
// case-insensitive lookup callers rely on.
//
// Returning nil signals "verification passed (or disabled)";
// returning a non-nil error signals "reject the request with 401".
// Connectors that don't implement WebhookVerifier are accepted
// without signature checks (current behaviour).
type WebhookVerifier interface {
	VerifyWebhookRequest(headers map[string][]string, payload []byte) error
}

// Grant represents a permission grant that a Provisioner pushes back to
// the upstream source. Used by sources that allow the platform to manage
// their access lists (e.g. SharePoint, Notion).
type Grant struct {
	// PrincipalID is the upstream identifier of the user or group to
	// grant / revoke.
	PrincipalID string

	// PrincipalType narrows the principal kind ("user", "group",
	// "service-account").
	PrincipalType string

	// ResourceID is the connector-native identifier of the resource
	// being granted (folder, page, channel).
	ResourceID string

	// Permission names the role being granted ("reader", "editor", ...).
	// The connector defines the allowed values.
	Permission string
}

// Provisioner is implemented by connectors that can push permission
// changes upstream. Connectors that only read leave this unimplemented;
// the platform falls back to "request access" UX in the admin portal.
type Provisioner interface {
	// Provision adds the supplied grants on the upstream source.
	// Implementations must be idempotent — re-applying an existing grant
	// must not fail.
	Provision(ctx context.Context, conn Connection, grants []Grant) error

	// Deprovision removes the supplied grants. Removing a grant that
	// does not exist must not fail.
	Deprovision(ctx context.Context, conn Connection, grants []Grant) error
}
