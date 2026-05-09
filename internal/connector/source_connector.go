// Package connector defines the contract every source connector implements
// (SharePoint, Google Drive, Slack, Notion, ...). The interface mirrors the
// pattern used in uneycom/ai-agent-platform's connector layer so a connector
// can move between repos without re-numbering.
//
// The interface is deliberately narrow: it covers validation, lifecycle,
// namespace enumeration, document enumeration / fetch, and subscription. The
// optional capabilities (delta sync, webhook receive, provisioning) live in
// optional_interfaces.go and are detected via type assertion at runtime.
package connector

import (
	"context"
	"errors"
	"io"
	"time"
)

// ConnectorConfig is the static, persisted configuration for a connector
// instance. Credentials are stored separately (envelope-encrypted) and are
// supplied to Connect via the Credentials field on this struct after the
// caller decrypts them.
//
// Settings is opaque to the registry; each connector validates its own
// shape in Validate.
type ConnectorConfig struct {
	// Name identifies the connector implementation, e.g. "google-drive".
	// Must match the name used to register it in the global registry.
	Name string

	// TenantID is the multi-tenant scope this connection lives in.
	TenantID string

	// SourceID is the platform-side identifier for this specific source
	// instance. Multiple Google Drive connections under the same tenant
	// each have their own SourceID.
	SourceID string

	// Settings holds connector-specific configuration (e.g. a SharePoint
	// site URL, a Slack workspace ID). The connector defines and validates
	// the shape.
	Settings map[string]any

	// Credentials are the decrypted bytes the connector needs to talk to
	// the upstream API. The control plane is responsible for envelope-
	// decrypting these (see internal/credential) before calling Connect.
	Credentials []byte
}

// Connection is an opaque handle returned by Connect. Connector
// implementations cast back to their own concrete type internally; callers
// only pass it through the rest of the SourceConnector API.
type Connection interface {
	// TenantID identifies which tenant this connection belongs to. Used by
	// the platform backend to enforce tenant isolation around every call.
	TenantID() string

	// SourceID identifies the specific source instance this connection
	// represents. Stable across reconnects.
	SourceID() string
}

// Namespace is a scoping unit inside a source — e.g. a Google Drive shared
// drive, a SharePoint site, a Slack channel. A single connection may have
// many namespaces; the platform decides which ones to ingest.
type Namespace struct {
	// ID is the connector-native identifier for the namespace (drive ID,
	// channel ID, site ID, ...). Must be stable across calls.
	ID string

	// Name is the human-readable label shown in the admin portal.
	Name string

	// Kind classifies the namespace ("drive", "channel", "space", ...).
	// Used by retrieval to filter, not interpreted by the platform.
	Kind string

	// Metadata carries connector-specific extras (parent IDs, URLs, ...).
	Metadata map[string]string
}

// DocumentRef is a stable pointer to a document in the source. The
// connector returns these from ListDocuments and accepts them in
// FetchDocument.
type DocumentRef struct {
	// NamespaceID scopes the reference to a namespace.
	NamespaceID string

	// ID is the connector-native document identifier.
	ID string

	// ETag, if present, lets callers detect whether the document changed
	// without fetching the body. Connectors that don't expose etags leave
	// it empty.
	ETag string

	// UpdatedAt is the upstream "last modified" timestamp, when known.
	UpdatedAt time.Time
}

// Document is a fetched document plus the metadata the platform needs to
// route it through the ingestion pipeline. Content is exposed as a Reader
// so connectors can stream large objects without buffering them in memory.
type Document struct {
	Ref DocumentRef

	// MIMEType is the best guess at the content type ("application/pdf",
	// "text/markdown", ...).
	MIMEType string

	// Title is the human-readable document title.
	Title string

	// Author is the upstream author / owner email or display name.
	Author string

	// Size is the document size in bytes when known. Zero means unknown.
	Size int64

	// CreatedAt and UpdatedAt are upstream timestamps.
	CreatedAt time.Time
	UpdatedAt time.Time

	// Metadata carries connector-specific extras (path, URL, labels).
	Metadata map[string]string

	// Content streams the raw document bytes. Callers must Close it when
	// finished. May be nil for metadata-only fetches.
	Content io.ReadCloser
}

// ListOpts narrows ListDocuments. Connectors that don't support a given
// option ignore it.
type ListOpts struct {
	// PageSize is the requested page size. Zero means connector default.
	PageSize int

	// PageToken is the opaque cursor returned by the previous page; empty
	// for the first page.
	PageToken string

	// Since, when non-zero, asks the connector to return only documents
	// updated at or after this timestamp.
	Since time.Time
}

// DocumentIterator iterates documents lazily. Implementations may make
// network calls in Next; callers should respect ctx cancellation by
// breaking out of the loop.
//
// Typical usage:
//
//	for it.Next(ctx) {
//	    ref := it.Doc()
//	    // ...
//	}
//	if err := it.Err(); err != nil { ... }
//	_ = it.Close()
type DocumentIterator interface {
	Next(ctx context.Context) bool
	Doc() DocumentRef
	Err() error
	Close() error
}

// Subscription is a long-lived handle to upstream change events. The
// connector pushes DocumentChange values into the Events channel until
// Close is called or the underlying stream errors out.
type Subscription interface {
	Events() <-chan DocumentChange
	Err() error
	Close() error
}

// ChangeKind classifies the type of change a connector observed.
type ChangeKind int

const (
	// ChangeUnknown is the zero value; treat as "rescan needed".
	ChangeUnknown ChangeKind = iota
	// ChangeUpserted means the document was added or modified.
	ChangeUpserted
	// ChangeDeleted means the document was removed at the source.
	ChangeDeleted
)

// DocumentChange is one upstream change event. Used by both Subscribe
// (push) and the optional DeltaSyncer interface (pull).
type DocumentChange struct {
	Kind ChangeKind
	Ref  DocumentRef
}

// SourceConnector is the contract every source-system integration
// implements. The methods are grouped by lifecycle:
//
//	Lifecycle:    Validate, Connect, Disconnect
//	Discovery:    ListNamespaces, ListDocuments, FetchDocument
//	Realtime:     Subscribe
//
// Methods must be safe to call concurrently with their Connection unless
// the connector documents otherwise. Implementations should respect ctx
// cancellation and return ctx.Err() when the caller cancels.
type SourceConnector interface {
	// Validate checks whether cfg is well-formed for this connector, before
	// any network call is made. Returns ErrInvalidConfig (or wraps it) if
	// the config is rejected.
	Validate(ctx context.Context, cfg ConnectorConfig) error

	// Connect opens a session against the upstream source and returns a
	// Connection. Implementations should perform a cheap auth check (e.g.
	// "who am I") before returning.
	Connect(ctx context.Context, cfg ConnectorConfig) (Connection, error)

	// ListNamespaces returns the namespaces visible to the connection,
	// e.g. shared drives, channels, sites.
	ListNamespaces(ctx context.Context, conn Connection) ([]Namespace, error)

	// ListDocuments enumerates documents inside ns. Pagination is driven
	// by ListOpts.PageToken; the iterator returns ErrEndOfPage once all
	// pages are exhausted.
	ListDocuments(ctx context.Context, conn Connection, ns Namespace, opts ListOpts) (DocumentIterator, error)

	// FetchDocument returns the full document body and metadata for ref.
	// Callers must Close the returned Document.Content.
	FetchDocument(ctx context.Context, conn Connection, ref DocumentRef) (*Document, error)

	// Subscribe opens a long-lived change stream for ns. Connectors that
	// only support polling return ErrNotSupported and rely on the optional
	// DeltaSyncer interface instead.
	Subscribe(ctx context.Context, conn Connection, ns Namespace) (Subscription, error)

	// Disconnect closes the connection and releases any upstream session
	// state.
	Disconnect(ctx context.Context, conn Connection) error
}

// ErrInvalidConfig is returned by Validate when the supplied config is
// rejected. Connectors should wrap a more specific cause with
// fmt.Errorf("%w: ...", ErrInvalidConfig, ...).
var ErrInvalidConfig = errors.New("connector: invalid config")

// ErrNotSupported is returned by methods a connector cannot satisfy
// (typically Subscribe on poll-only sources).
var ErrNotSupported = errors.New("connector: capability not supported")

// ErrEndOfPage is returned by DocumentIterator.Err when the iterator
// finished cleanly. Callers should check `errors.Is(err, ErrEndOfPage)`
// rather than treating it as a hard failure.
var ErrEndOfPage = errors.New("connector: end of page")
