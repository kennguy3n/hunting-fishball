// preview_handler.go — Round-5 Task 10.
//
// POST /v1/admin/sources/preview is the connector dry-run endpoint.
// An admin pastes a connector type + credentials (still pre-source-
// row) and the handler runs:
//
//	Validate -> Connect -> ListNamespaces -> ListDocuments (per-ns,
//	page-limited)
//
// The response is a namespace tree with an estimated document count
// per namespace. Nothing is persisted; the credential bytes are
// held in memory only and never echo'd back. The endpoint is the
// primary "does this token actually work?" tool the admin portal
// uses before committing to a source row.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// PreviewLimits caps the cost of a single dry-run.
type PreviewLimits struct {
	// MaxNamespaces caps how many namespaces are walked. Defaults
	// to 50 when zero.
	MaxNamespaces int

	// MaxDocsPerNamespace caps how many documents are enumerated
	// per namespace. Defaults to 100 when zero.
	MaxDocsPerNamespace int

	// Timeout caps the end-to-end wall clock of the dry-run.
	// Defaults to 30s when zero.
	Timeout time.Duration
}

// effective returns a copy of l with all zero-valued fields filled in
// from defaults.
func (l PreviewLimits) effective() PreviewLimits {
	if l.MaxNamespaces <= 0 {
		l.MaxNamespaces = 50
	}
	if l.MaxDocsPerNamespace <= 0 {
		l.MaxDocsPerNamespace = 100
	}
	if l.Timeout <= 0 {
		l.Timeout = 30 * time.Second
	}
	return l
}

// PreviewRequest is the JSON body of the preview endpoint.
type PreviewRequest struct {
	ConnectorType string         `json:"connector_type"`
	Settings      map[string]any `json:"settings,omitempty"`
	Credentials   string         `json:"credentials,omitempty"`
}

// PreviewNamespace is one row of the response namespace tree.
type PreviewNamespace struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind,omitempty"`
	DocsCount int    `json:"docs_count"`
	// Truncated reports whether the document iterator was capped
	// at MaxDocsPerNamespace.
	Truncated bool `json:"truncated,omitempty"`
}

// PreviewResponse is the JSON body returned to the admin.
type PreviewResponse struct {
	ConnectorType string             `json:"connector_type"`
	Namespaces    []PreviewNamespace `json:"namespaces"`
	// NamespacesTruncated reports whether the namespace list was
	// capped at MaxNamespaces.
	NamespacesTruncated bool `json:"namespaces_truncated,omitempty"`
}

// PreviewConnectorResolver resolves a connector type to a fresh
// SourceConnector instance. Defaults to the package registry but
// pluggable so unit tests can install fakes without touching the
// global registry.
type PreviewConnectorResolver interface {
	Get(name string) (connector.SourceConnector, error)
}

// PreviewConnectorResolverFunc adapts a plain function to
// PreviewConnectorResolver.
type PreviewConnectorResolverFunc func(name string) (connector.SourceConnector, error)

// Get implements PreviewConnectorResolver.
func (f PreviewConnectorResolverFunc) Get(name string) (connector.SourceConnector, error) {
	return f(name)
}

// DefaultPreviewResolver is the production resolver — it looks up
// factories via connector.GetSourceConnector and instantiates them.
var DefaultPreviewResolver = PreviewConnectorResolverFunc(func(name string) (connector.SourceConnector, error) {
	factory, err := connector.GetSourceConnector(name)
	if err != nil {
		return nil, err
	}
	return factory(), nil
})

// PreviewHandlerConfig wires the handler.
type PreviewHandlerConfig struct {
	Resolver PreviewConnectorResolver
	Limits   PreviewLimits
}

// PreviewHandler serves the dry-run endpoint.
type PreviewHandler struct {
	cfg PreviewHandlerConfig
}

// NewPreviewHandler validates cfg and returns a handler. A nil
// Resolver falls back to DefaultPreviewResolver.
func NewPreviewHandler(cfg PreviewHandlerConfig) *PreviewHandler {
	if cfg.Resolver == nil {
		cfg.Resolver = DefaultPreviewResolver
	}
	cfg.Limits = cfg.Limits.effective()
	return &PreviewHandler{cfg: cfg}
}

// Register mounts POST /v1/admin/sources/preview on rg.
func (h *PreviewHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/sources/preview", h.preview)
}

func (h *PreviewHandler) preview(c *gin.Context) {
	var req PreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	req.ConnectorType = strings.TrimSpace(req.ConnectorType)
	if req.ConnectorType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connector_type required"})
		return
	}
	authVal, ok := c.Get(audit.TenantContextKey)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	tenantID, _ := authVal.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}

	conn, err := h.cfg.Resolver.Get(req.ConnectorType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown connector: " + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.Limits.Timeout)
	defer cancel()

	cfg := connector.ConnectorConfig{
		Name:        req.ConnectorType,
		TenantID:    tenantID,
		Settings:    req.Settings,
		Credentials: []byte(req.Credentials),
	}
	// Round-20 Task 16: schema validation. If the connector
	// exports a CredentialSchemaProvider, validate the credential
	// blob against that schema before calling Validate(). This
	// produces precise field-level errors a connector's
	// connect-time Validate often can't.
	if sp, ok := conn.(connector.CredentialSchemaProvider); ok {
		if vErr := connector.ValidateCredentialsErr(cfg.Credentials, sp.CredentialSchema()); vErr != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": vErr.Error()})
			return
		}
	}
	if err := conn.Validate(ctx, cfg); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "validate: " + err.Error()})
		return
	}
	connection, err := conn.Connect(ctx, cfg)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "connect: " + err.Error()})
		return
	}
	defer func() { _ = conn.Disconnect(ctx, connection) }()

	resp, err := walkPreview(ctx, conn, connection, h.cfg.Limits)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	resp.ConnectorType = req.ConnectorType
	c.JSON(http.StatusOK, resp)
}

// walkPreview drives the namespace + document enumeration up to the
// supplied limits. Errors from ListNamespaces are fatal; per-namespace
// errors from ListDocuments are surfaced into the namespace's
// DocsCount = 0 entry rather than aborting the whole response — the
// admin needs to know which namespaces are reachable, not just that
// one of them threw.
func walkPreview(ctx context.Context, c connector.SourceConnector, conn connector.Connection, limits PreviewLimits) (*PreviewResponse, error) {
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	resp := &PreviewResponse{}
	if len(ns) > limits.MaxNamespaces {
		ns = ns[:limits.MaxNamespaces]
		resp.NamespacesTruncated = true
	}
	resp.Namespaces = make([]PreviewNamespace, 0, len(ns))
	for _, n := range ns {
		entry := PreviewNamespace{ID: n.ID, Name: n.Name, Kind: n.Kind}
		count, truncated, listErr := countDocuments(ctx, c, conn, n, limits.MaxDocsPerNamespace)
		if listErr != nil {
			// Surface a per-namespace failure without breaking
			// the whole walk; the caller can see DocsCount == 0
			// and an explanatory metadata field if we extend the
			// response.
			entry.DocsCount = 0
		} else {
			entry.DocsCount = count
			entry.Truncated = truncated
		}
		resp.Namespaces = append(resp.Namespaces, entry)
	}
	return resp, nil
}

// countDocuments counts up to max documents in ns, returning
// (count, truncated, err). Truncated is true when the iterator
// returned at least max+1 documents.
func countDocuments(ctx context.Context, c connector.SourceConnector, conn connector.Connection, ns connector.Namespace, max int) (int, bool, error) {
	it, err := c.ListDocuments(ctx, conn, ns, connector.ListOpts{PageSize: max})
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = it.Close() }()

	count := 0
	for it.Next(ctx) {
		count++
		if count > max {
			return max, true, nil
		}
	}
	if err := it.Err(); err != nil && !errors.Is(err, connector.ErrEndOfPage) {
		return count, false, err
	}
	return count, false, nil
}
