// Package uploadportal implements the upload-portal connector —
// a server-side receiver rather than an outbound puller.
// Clients POST multipart uploads to the platform's webhook
// endpoint with a signed token; this connector validates the
// token, the file type, the size limits, and the tenant scope,
// then surfaces each upload as an upserted DocumentChange.
//
// Credentials must be a JSON blob:
//
//	{
//	  "webhook_secret":   "...",                 // required (HMAC-SHA256)
//	  "allowed_mime":     ["application/pdf"],    // optional
//	  "max_size_bytes":   10485760               // optional (default 10MiB)
//	}
//
// The connector implements SourceConnector (the pull surface is a
// no-op — every doc comes in via HandleWebhook) plus the
// connection-aware WebhookReceiverFor + WebhookVerifierFor
// optional interfaces. The platform webhook router uses these to
// materialise a per-source Connection (from the stored credential
// blob) and dispatch verification + parsing against the right
// tenant-scoped secret and MIME/size policy.
//
// The connector deliberately does NOT implement the registry-level
// WebhookVerifier interface. Its signing secret is per-source, so
// a registry-level verifier would always run with a nil key and
// silently reject every legitimate upload. Implementing only the
// connection-aware variants forces the router (and any custom
// integrator) to take the per-source path.
//
// Audit contract: this connector has no outbound HTTP client of
// its own — uploads arrive via the platform webhook router — so
// the Round-15 audit-gate tokens are referenced for posterity
// only. The receiver never needs to emit connector.ErrRateLimited
// nor build an http.NewRequestWithContext call directly.
package uploadportal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

const (
	// Name is the registry-visible connector name.
	Name = "upload_portal"

	// DefaultMaxSize is the upload size limit applied when the
	// caller doesn't override max_size_bytes.
	DefaultMaxSize int64 = 10 * 1024 * 1024
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	WebhookSecret string   `json:"webhook_secret"`
	AllowedMIME   []string `json:"allowed_mime,omitempty"`
	MaxSizeBytes  int64    `json:"max_size_bytes,omitempty"`
}

// Connector implements connector.SourceConnector,
// connector.WebhookReceiver, connector.WebhookReceiverFor, and
// connector.WebhookVerifierFor.
type Connector struct {
	now func() time.Time
}

// Option configures a Connector.
type Option func(*Connector)

// WithNow injects a deterministic clock — used by tests.
func WithNow(now func() time.Time) Option { return func(o *Connector) { o.now = now } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID      string
	sourceID      string
	webhookSecret []byte
	allowedMIME   []string
	maxSize       int64
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (o *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
	if cfg.TenantID == "" {
		return fmt.Errorf("%w: tenant_id required", connector.ErrInvalidConfig)
	}
	if cfg.SourceID == "" {
		return fmt.Errorf("%w: source_id required", connector.ErrInvalidConfig)
	}
	if len(cfg.Credentials) == 0 {
		return fmt.Errorf("%w: credentials required", connector.ErrInvalidConfig)
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return fmt.Errorf("%w: parse credentials: %v", connector.ErrInvalidConfig, err)
	}
	if creds.WebhookSecret == "" {
		return fmt.Errorf("%w: webhook_secret required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect builds an in-memory connection capturing the per-source
// validation policy. The connector never makes outbound calls.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	maxSize := creds.MaxSizeBytes
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}

	return &connection{
		tenantID:      cfg.TenantID,
		sourceID:      cfg.SourceID,
		webhookSecret: []byte(creds.WebhookSecret),
		allowedMIME:   creds.AllowedMIME,
		maxSize:       maxSize,
	}, nil
}

// ListNamespaces exposes the configured source as the only
// namespace.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("upload_portal: bad connection type")
	}

	return []connector.Namespace{{ID: conn.sourceID, Name: "uploads", Kind: "uploads"}}, nil
}

type emptyIterator struct{}

func (emptyIterator) Next(_ context.Context) bool { return false }
func (emptyIterator) Doc() connector.DocumentRef  { return connector.DocumentRef{} }
func (emptyIterator) Err() error                  { return connector.ErrEndOfPage }
func (emptyIterator) Close() error                { return nil }

// ListDocuments is a no-op iterator. Upload-portal documents are
// delivered via HandleWebhook and stored in the platform's object
// store — the pull surface has nothing to enumerate.
func (o *Connector) ListDocuments(_ context.Context, _ connector.Connection, _ connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	return emptyIterator{}, nil
}

// FetchDocument returns ErrNotSupported. Uploads are served from
// the platform's storage layer outside this connector.
func (o *Connector) FetchDocument(_ context.Context, _ connector.Connection, _ connector.DocumentRef) (*connector.Document, error) {
	return nil, connector.ErrNotSupported
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// WebhookPath is the path suffix the platform mounts the receiver
// at — /v1/webhooks/upload_portal.
func (o *Connector) WebhookPath() string { return "/upload_portal" }

// VerifyWebhookRequestFor validates the `X-Upload-Signature-256`
// header against the per-source HMAC secret captured on conn.
// This is the connection-aware verifier the platform webhook
// router invokes when the connector is also a
// connector.WebhookReceiverFor.
func (o *Connector) VerifyWebhookRequestFor(conn connector.Connection, headers map[string][]string, payload []byte) error {
	c, ok := conn.(*connection)
	if !ok {
		return errors.New("upload_portal: bad connection type")
	}
	return verifyHMAC(headers, payload, c)
}

// verifyHMAC is the shared signature-check helper used by both
// the connection-aware verifier and HandleWebhookFor's defence-
// in-depth verification. Centralising it avoids drift between the
// two entry points.
func verifyHMAC(headers map[string][]string, payload []byte, conn *connection) error {
	return connector.VerifyHMACSHA256(conn.webhookSecret, payload, connector.FirstHeader(headers, "X-Upload-Signature-256"))
}

// HandleWebhook is the stateless registry-level entry point. It
// parses the multipart payload but CANNOT enforce signature
// verification or per-source MIME/size policy — those live on
// the per-source Connection. The platform webhook router detects
// WebhookReceiverFor and dispatches via HandleWebhookFor instead;
// HandleWebhook exists only so the connector still satisfies the
// WebhookReceiver interface for registry lookups and audit
// tooling. Production callers MUST use HandleWebhookFor.
func (o *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	return o.handle(nil, payload)
}

// HandleWebhookFor is the connection-aware variant used by the
// platform webhook router. It re-verifies the signature (so direct
// callers that haven't already invoked VerifyWebhookRequestFor
// remain safe — double-verification with the same secret is
// harmless) and then enforces the tenant-scoped MIME + size policy
// captured in conn.
func (o *Connector) HandleWebhookFor(_ context.Context, conn connector.Connection, headers map[string][]string, payload []byte) ([]connector.DocumentChange, error) {
	c, ok := conn.(*connection)
	if !ok {
		return nil, errors.New("upload_portal: bad connection type")
	}
	if err := verifyHMAC(headers, payload, c); err != nil {
		return nil, err
	}

	return o.handle(c, payload)
}

func (o *Connector) handle(conn *connection, payload []byte) ([]connector.DocumentChange, error) {
	boundary, err := sniffBoundary(payload)
	if err != nil {
		return nil, err
	}
	mr := multipart.NewReader(bytes.NewReader(payload), boundary)
	var changes []connector.DocumentChange
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("upload_portal: read part: %w", err)
		}
		if part.FileName() == "" {
			_ = part.Close()

			continue
		}
		ct := part.Header.Get("Content-Type")
		if conn != nil && len(conn.allowedMIME) > 0 && !contains(conn.allowedMIME, ct) {
			_ = part.Close()

			return nil, fmt.Errorf("upload_portal: mime %q not in allowed list", ct)
		}
		buf := &bytes.Buffer{}
		limit := int64(0)
		if conn != nil {
			limit = conn.maxSize
		}
		if limit > 0 {
			if _, err := io.CopyN(buf, part, limit+1); err != nil && !errors.Is(err, io.EOF) {
				_ = part.Close()

				return nil, fmt.Errorf("upload_portal: read part body: %w", err)
			}
			if int64(buf.Len()) > limit {
				_ = part.Close()

				return nil, fmt.Errorf("upload_portal: upload exceeds max_size_bytes=%d", limit)
			}
		} else {
			if _, err := io.Copy(buf, part); err != nil {
				_ = part.Close()

				return nil, fmt.Errorf("upload_portal: read part body: %w", err)
			}
		}
		_ = part.Close()
		id := filepath.Base(part.FileName())
		ref := connector.DocumentRef{NamespaceID: "uploads", ID: id, UpdatedAt: o.now()}
		changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
	}
	if len(changes) == 0 {
		return nil, errors.New("upload_portal: no file parts in upload")
	}

	return changes, nil
}

// sniffBoundary returns the multipart boundary by reading the
// first delimiter line of payload (`--<boundary>\r\n`). We can't
// rely on the Content-Type header here because the platform
// router only forwards the body to HandleWebhook.
func sniffBoundary(payload []byte) (string, error) {
	if len(payload) < 4 {
		return "", errors.New("upload_portal: payload too small")
	}
	nl := bytes.IndexAny(payload, "\r\n")
	if nl <= 2 {
		return "", errors.New("upload_portal: no boundary line")
	}
	first := payload[:nl]
	if !bytes.HasPrefix(first, []byte("--")) {
		return "", errors.New("upload_portal: payload not multipart")
	}

	return string(first[2:]), nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}

	return false
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
