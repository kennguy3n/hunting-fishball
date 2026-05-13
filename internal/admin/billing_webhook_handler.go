// billing_webhook_handler.go — Round-24 Task 24.
//
// POST /v1/admin/webhooks/billing registers a tenant-scoped HMAC-
// signed webhook the external billing system uses to subscribe to
// usage events (per-tenant retrieval count, per-tenant ingest
// document count). The endpoint is intentionally narrow:
//
//   - Tenants register at most one billing webhook (re-register
//     to rotate the URL or shared secret).
//   - Re-registration is idempotent: an existing subscription is
//     overwritten with the new URL + secret.
//
// The actual emit path (publishing to the registered URL) is a
// future task — Round-24 lands the endpoint + storage seam so the
// billing team can integrate in parallel.

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// BillingWebhookSubscription is the persisted shape.
type BillingWebhookSubscription struct {
	TenantID     string
	URL          string
	SharedSecret string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// BillingWebhookStore is the storage seam.
type BillingWebhookStore interface {
	Upsert(ctx context.Context, sub BillingWebhookSubscription) error
	Get(ctx context.Context, tenantID string) (*BillingWebhookSubscription, error)
	Delete(ctx context.Context, tenantID string) error
}

// BillingWebhookHandler serves the admin endpoint.
type BillingWebhookHandler struct {
	store BillingWebhookStore
	audit AuditWriter
	now   func() time.Time
}

// NewBillingWebhookHandler validates inputs and returns the handler.
func NewBillingWebhookHandler(store BillingWebhookStore, aw AuditWriter) (*BillingWebhookHandler, error) {
	if store == nil {
		return nil, errors.New("billing webhook: nil store")
	}
	if aw == nil {
		aw = noopAudit{}
	}

	return &BillingWebhookHandler{store: store, audit: aw, now: time.Now}, nil
}

// Register mounts the routes.
//
//	POST   /v1/admin/webhooks/billing  — upsert subscription
//	GET    /v1/admin/webhooks/billing  — fetch current subscription
//	DELETE /v1/admin/webhooks/billing  — unregister
func (h *BillingWebhookHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/webhooks/billing", h.post)
	rg.GET("/v1/admin/webhooks/billing", h.get)
	rg.DELETE("/v1/admin/webhooks/billing", h.delete)
}

// BillingWebhookRequest is the JSON request shape.
type BillingWebhookRequest struct {
	URL          string `json:"url" binding:"required"`
	SharedSecret string `json:"shared_secret" binding:"required"`
}

// BillingWebhookResponse is the JSON response shape. The secret
// is intentionally never echoed back — callers store it in the
// secret manager and we treat it as write-once.
type BillingWebhookResponse struct {
	TenantID  string    `json:"tenant_id"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (h *BillingWebhookHandler) post(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}
	var req BillingWebhookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}
	parsed, err := url.Parse(req.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url must be https://"})

		return
	}
	if len(strings.TrimSpace(req.SharedSecret)) < 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "shared_secret must be at least 32 chars"})

		return
	}
	now := h.now().UTC()
	sub := BillingWebhookSubscription{
		TenantID:     tenantID,
		URL:          req.URL,
		SharedSecret: req.SharedSecret,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if existing, err := h.store.Get(c.Request.Context(), tenantID); err == nil && existing != nil {
		sub.CreatedAt = existing.CreatedAt
	}
	if err := h.store.Upsert(c.Request.Context(), sub); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

		return
	}
	actor, _ := c.Get(audit.ActorContextKey)
	actorID, _ := actor.(string)
	log := audit.NewAuditLog(tenantID, actorID, audit.ActionBillingWebhookRegistered, "billing_webhook", tenantID,
		audit.JSONMap{"url": req.URL}, "")
	_ = h.audit.Create(c.Request.Context(), log)
	c.JSON(http.StatusOK, BillingWebhookResponse{TenantID: tenantID, URL: req.URL, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt})
}

func (h *BillingWebhookHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}
	sub, err := h.store.Get(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

		return
	}
	if sub == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no billing webhook registered"})

		return
	}
	c.JSON(http.StatusOK, BillingWebhookResponse{TenantID: sub.TenantID, URL: sub.URL, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt})
}

func (h *BillingWebhookHandler) delete(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}
	if err := h.store.Delete(c.Request.Context(), tenantID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

		return
	}
	actor, _ := c.Get(audit.ActorContextKey)
	actorID, _ := actor.(string)
	log := audit.NewAuditLog(tenantID, actorID, audit.ActionBillingWebhookUnregistered, "billing_webhook", tenantID, audit.JSONMap{}, "")
	_ = h.audit.Create(c.Request.Context(), log)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
