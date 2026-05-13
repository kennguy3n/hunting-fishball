// Package admin — webhook_router.go ships a generic HTTP front
// door that maps inbound webhooks to the right connector instance
// without each connector having to register its own Gin route.
//
// Round-5 Task 5: per-connector webhook plumbing was bespoke up
// to PR #13 — Slack registered its own gin route, Google Drive
// hadn't been wired yet, and Notion had a half-implemented
// receiver. This router puts every connector that implements
// WebhookReceiver behind a single uniform path:
//
//	POST /v1/webhooks/:connector/:source_id
//
// The router resolves the source from SourceRepository (scoped by
// the path-supplied source_id and the connector type), looks up
// the connector factory in the registry, asserts it implements
// WebhookReceiver (and optionally WebhookVerifier), runs
// signature verification with the source's stored secret, and
// then delegates to the connector's HandleWebhook.
//
// Audit events are emitted via internal/audit/Action constants
// (`webhook.received`, `webhook.processed`, `webhook.failed`) so
// SREs get a single timeline of webhook activity per source.
package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// WebhookRouter routes inbound webhooks to per-connector
// HandleWebhook implementations.
type WebhookRouter struct {
	cfg WebhookRouterConfig
}

// WebhookRouterConfig wires the router's dependencies. Lookup is
// the narrow read contract the router needs from
// SourceRepository.
//
// Resolver returns the registry-level WebhookReceiver projection;
// SourceResolver returns the underlying SourceConnector so the
// router can call Connect(ctx, cfg) when the connector advertises
// connector.WebhookReceiverFor. Credentials extracts the
// decrypted credential bytes from a Source row. Both have
// production defaults that read from the global registry and the
// Source.Config["credentials"] JSONB key respectively.
type WebhookRouterConfig struct {
	Lookup ConnectorLookup
	// Resolver maps a connector name + a SourceConnector instance
	// produced by the factory. Tests inject a fake; production
	// uses ResolveFromRegistry.
	Resolver ConnectorResolver
	// SourceResolver is the connection-aware sibling of Resolver. It
	// returns a fresh SourceConnector instance so the router can
	// call Connect when the connector implements WebhookReceiverFor.
	// Defaults to SourceResolveFromRegistry.
	SourceResolver SourceConnectorResolver
	// Credentials extracts the decrypted credential blob from a
	// Source row, in the shape ConnectorConfig.Credentials expects.
	// Defaults to DefaultCredentialExtractor.
	Credentials CredentialExtractor
	Audit       AuditWriter
}

// ConnectorLookup is the narrow read surface that resolves a
// (tenant_id, source_id) tuple to its database row. Tenants are
// pulled from the row, NOT the request, so a hostile client can't
// drive a webhook against another tenant's source by guessing the
// id.
type ConnectorLookup interface {
	// GetByID resolves a Source by its primary key alone (the
	// usual tenant-scoped Get is on the wrong axis here — webhook
	// callers don't know the tenant and don't need to).
	GetByID(ctx context.Context, sourceID string) (*Source, error)
}

// ConnectorResolver builds a SourceConnector instance for a given
// connector type, returning the WebhookReceiver projection if
// supported. Production wiring delegates to the connector
// registry.
type ConnectorResolver func(connectorType string) (connector.WebhookReceiver, error)

// SourceConnectorResolver returns the underlying SourceConnector
// instance for a connector type — the connection-aware sibling of
// ConnectorResolver. The router calls this when the connector
// implements connector.WebhookReceiverFor and a Connection has to
// be materialised before verification + handling can run.
type SourceConnectorResolver func(connectorType string) (connector.SourceConnector, error)

// CredentialExtractor pulls the decrypted credential bytes out of
// a Source row in a form ConnectorConfig.Credentials accepts.
// Production wiring decrypts the envelope-encrypted blob from
// Source.Config["credentials"]; tests can install a deterministic
// fake.
type CredentialExtractor func(src *Source) ([]byte, error)

// ResolveFromRegistry is the production-wired ConnectorResolver.
// It calls connector.GetSourceConnector to materialise a fresh
// instance and asserts it implements WebhookReceiver.
func ResolveFromRegistry(connectorType string) (connector.WebhookReceiver, error) {
	factory, err := connector.GetSourceConnector(connectorType)
	if err != nil {
		return nil, err
	}
	c := factory()
	rcv, ok := c.(connector.WebhookReceiver)
	if !ok {
		return nil, ErrConnectorDoesNotReceiveWebhooks
	}
	return rcv, nil
}

// SourceResolveFromRegistry is the production-wired
// SourceConnectorResolver. It returns the factory's SourceConnector
// directly so the router can call Connect / Disconnect against it.
func SourceResolveFromRegistry(connectorType string) (connector.SourceConnector, error) {
	factory, err := connector.GetSourceConnector(connectorType)
	if err != nil {
		return nil, err
	}
	return factory(), nil
}

// DefaultCredentialExtractor reads the credentials out of
// Source.Config["credentials"]. GORM persists []byte values into
// JSONB columns as base64-encoded JSON strings, so the extractor
// transparently base64-decodes string-typed values while still
// accepting raw []byte (test fakes) and structured map/slice
// payloads (legacy callers that stored an unwrapped JSON object).
func DefaultCredentialExtractor(src *Source) ([]byte, error) {
	if src == nil {
		return nil, nil
	}
	v, ok := src.Config["credentials"]
	if !ok || v == nil {
		return nil, nil
	}
	switch c := v.(type) {
	case []byte:
		return c, nil
	case string:
		if decoded, err := base64.StdEncoding.DecodeString(c); err == nil {
			return decoded, nil
		}
		return []byte(c), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("admin: marshal credentials: %w", err)
		}
		return b, nil
	}
}

// ErrConnectorDoesNotReceiveWebhooks is returned by
// ResolveFromRegistry when the requested connector type does not
// implement the optional WebhookReceiver interface.
var ErrConnectorDoesNotReceiveWebhooks = errors.New("admin: connector does not receive webhooks")

// NewWebhookRouter validates the supplied config and returns a
// router. Resolver defaults to ResolveFromRegistry,
// SourceResolver to SourceResolveFromRegistry, Credentials to
// DefaultCredentialExtractor, and Audit to a no-op writer so
// production wiring is concise but tests can opt-in to a recording
// fake.
func NewWebhookRouter(cfg WebhookRouterConfig) (*WebhookRouter, error) {
	if cfg.Lookup == nil {
		return nil, errors.New("admin: WebhookRouterConfig.Lookup is required")
	}
	if cfg.Resolver == nil {
		cfg.Resolver = ResolveFromRegistry
	}
	if cfg.SourceResolver == nil {
		cfg.SourceResolver = SourceResolveFromRegistry
	}
	if cfg.Credentials == nil {
		cfg.Credentials = DefaultCredentialExtractor
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	return &WebhookRouter{cfg: cfg}, nil
}

// Register mounts the webhook handler. The path
// /v1/webhooks/:connector/:source_id is intentionally outside
// /v1/admin because callers are upstream SaaS providers that
// don't carry admin tokens — verification happens via the
// per-connector signature scheme.
func (r *WebhookRouter) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/webhooks/:connector/:source_id", r.handle)
}

func (r *WebhookRouter) handle(c *gin.Context) {
	connectorType := c.Param("connector")
	sourceID := c.Param("source_id")
	if connectorType == "" || sourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connector and source_id are required"})
		return
	}
	src, err := r.cfg.Lookup.GetByID(c.Request.Context(), sourceID)
	if errors.Is(err, ErrSourceNotFound) || src == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if src.ConnectorType != connectorType {
		// Defensive: the URL claims one connector but the row is
		// for another. Treat as not-found rather than 400 so the
		// caller can't probe for source-id existence by varying
		// the connector segment.
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	if src.Status != SourceStatusActive {
		c.JSON(http.StatusConflict, gin.H{"error": "source is not active"})
		return
	}
	rcv, err := r.cfg.Resolver(connectorType)
	if errors.Is(err, ErrConnectorDoesNotReceiveWebhooks) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	body, rerr := io.ReadAll(c.Request.Body)
	if rerr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + rerr.Error()})
		return
	}
	r.audit(c, src.TenantID, audit.ActionWebhookReceived, src.ID, audit.JSONMap{
		"connector": connectorType,
		"bytes":     len(body),
	})

	// Connection-aware dispatch path: when the connector advertises
	// WebhookReceiverFor, materialise a Connection from the stored
	// credentials and run verification + handling against it. This
	// is the only path that honours per-source secrets and policies
	// (e.g. upload_portal's per-tenant HMAC key + MIME/size limits).
	if rcvFor, ok := rcv.(connector.WebhookReceiverFor); ok {
		r.handlePerConnection(c, src, connectorType, body, rcvFor)
		return
	}

	if v, ok := rcv.(connector.WebhookVerifier); ok {
		if vErr := v.VerifyWebhookRequest(c.Request.Header, body); vErr != nil {
			r.audit(c, src.TenantID, audit.ActionWebhookFailed, src.ID, audit.JSONMap{
				"connector": connectorType,
				"reason":    "signature",
				"error":     vErr.Error(),
			})
			c.JSON(http.StatusUnauthorized, gin.H{"error": "signature verification failed"})
			return
		}
	}

	changes, hwErr := rcv.HandleWebhook(c.Request.Context(), body)
	if hwErr != nil {
		r.audit(c, src.TenantID, audit.ActionWebhookFailed, src.ID, audit.JSONMap{
			"connector": connectorType,
			"reason":    "handler",
			"error":     hwErr.Error(),
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": hwErr.Error()})
		return
	}
	r.audit(c, src.TenantID, audit.ActionWebhookProcessed, src.ID, audit.JSONMap{
		"connector": connectorType,
		"changes":   len(changes),
	})
	resp, _ := json.Marshal(gin.H{"status": "ok", "changes": len(changes)})
	c.Data(http.StatusAccepted, "application/json", resp)
}

// handlePerConnection drives the connection-aware dispatch path.
// It materialises a connector.Connection from the stored Source
// credentials, runs WebhookVerifierFor if implemented, and then
// invokes HandleWebhookFor. Verification failures are audited
// distinctly from handler failures so SREs can still slice the
// timeline by reason. Connect / extract failures are audited as
// "connection" so they're not mistaken for upstream-signature
// problems.
func (r *WebhookRouter) handlePerConnection(c *gin.Context, src *Source, connectorType string, body []byte, rcvFor connector.WebhookReceiverFor) {
	factory, err := r.cfg.SourceResolver(connectorType)
	if err != nil {
		r.audit(c, src.TenantID, audit.ActionWebhookFailed, src.ID, audit.JSONMap{
			"connector": connectorType,
			"reason":    "connection",
			"error":     err.Error(),
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "resolve source connector: " + err.Error()})
		return
	}
	creds, err := r.cfg.Credentials(src)
	if err != nil {
		r.audit(c, src.TenantID, audit.ActionWebhookFailed, src.ID, audit.JSONMap{
			"connector": connectorType,
			"reason":    "connection",
			"error":     err.Error(),
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "extract credentials: " + err.Error()})
		return
	}
	settings, _ := src.Config["settings"].(map[string]any)
	cfg := connector.ConnectorConfig{
		Name:        connectorType,
		TenantID:    src.TenantID,
		SourceID:    src.ID,
		Settings:    settings,
		Credentials: creds,
	}
	conn, err := factory.Connect(c.Request.Context(), cfg)
	if err != nil {
		r.audit(c, src.TenantID, audit.ActionWebhookFailed, src.ID, audit.JSONMap{
			"connector": connectorType,
			"reason":    "connection",
			"error":     err.Error(),
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "connect: " + err.Error()})
		return
	}
	defer func() { _ = factory.Disconnect(c.Request.Context(), conn) }()

	if vFor, ok := factory.(connector.WebhookVerifierFor); ok {
		if vErr := vFor.VerifyWebhookRequestFor(conn, c.Request.Header, body); vErr != nil {
			r.audit(c, src.TenantID, audit.ActionWebhookFailed, src.ID, audit.JSONMap{
				"connector": connectorType,
				"reason":    "signature",
				"error":     vErr.Error(),
			})
			c.JSON(http.StatusUnauthorized, gin.H{"error": "signature verification failed"})
			return
		}
	}

	changes, hwErr := rcvFor.HandleWebhookFor(c.Request.Context(), conn, c.Request.Header, body)
	if hwErr != nil {
		r.audit(c, src.TenantID, audit.ActionWebhookFailed, src.ID, audit.JSONMap{
			"connector": connectorType,
			"reason":    "handler",
			"error":     hwErr.Error(),
		})
		c.JSON(http.StatusInternalServerError, gin.H{"error": hwErr.Error()})
		return
	}
	r.audit(c, src.TenantID, audit.ActionWebhookProcessed, src.ID, audit.JSONMap{
		"connector": connectorType,
		"changes":   len(changes),
	})
	resp, _ := json.Marshal(gin.H{"status": "ok", "changes": len(changes)})
	c.Data(http.StatusAccepted, "application/json", resp)
}

func (r *WebhookRouter) audit(c *gin.Context, tenantID string, action audit.Action, resourceID string, meta audit.JSONMap) {
	log := audit.NewAuditLog(tenantID, "system:webhook", action, "source", resourceID, meta, "")
	_ = r.cfg.Audit.Create(c.Request.Context(), log)
}
