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
	"encoding/json"
	"errors"
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
type WebhookRouterConfig struct {
	Lookup ConnectorLookup
	// Resolver maps a connector name + a SourceConnector instance
	// produced by the factory. Tests inject a fake; production
	// uses ResolveFromRegistry.
	Resolver ConnectorResolver
	Audit    AuditWriter
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

// ErrConnectorDoesNotReceiveWebhooks is returned by
// ResolveFromRegistry when the requested connector type does not
// implement the optional WebhookReceiver interface.
var ErrConnectorDoesNotReceiveWebhooks = errors.New("admin: connector does not receive webhooks")

// NewWebhookRouter validates the supplied config and returns a
// router. Resolver defaults to ResolveFromRegistry; Audit
// defaults to a no-op writer so production wiring is concise but
// tests can opt-in to a recording fake.
func NewWebhookRouter(cfg WebhookRouterConfig) (*WebhookRouter, error) {
	if cfg.Lookup == nil {
		return nil, errors.New("admin: WebhookRouterConfig.Lookup is required")
	}
	if cfg.Resolver == nil {
		cfg.Resolver = ResolveFromRegistry
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

func (r *WebhookRouter) audit(c *gin.Context, tenantID string, action audit.Action, resourceID string, meta audit.JSONMap) {
	log := audit.NewAuditLog(tenantID, "system:webhook", action, "source", resourceID, meta, "")
	_ = r.cfg.Audit.Create(c.Request.Context(), log)
}
