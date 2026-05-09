package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// ConnectorValidator is the narrow contract the source handler needs
// from the connector registry. The default implementation calls
// connector.GetSourceConnector and the returned connector's
// Validate(); tests inject a fake.
type ConnectorValidator interface {
	Validate(ctx context.Context, connectorType string, cfg connector.ConnectorConfig) error
}

// AuditWriter is the narrow contract the source handler needs from
// the audit repository. Real wiring uses *audit.Repository; tests
// inject an in-memory recorder.
type AuditWriter interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// HealthReader is the narrow contract the GET /health handler needs.
// The real wiring is *HealthRepository; tests inject a fake.
type HealthReader interface {
	Get(ctx context.Context, tenantID, sourceID string) (*Health, error)
}

// EventEmitter is the narrow contract the source handler uses to
// publish a kick-off ingestion event when a source is connected /
// re-scoped. Real wiring uses *pipeline.Producer; tests inject a fake.
type EventEmitter interface {
	EmitSourceConnected(ctx context.Context, tenantID, sourceID, connectorType string) error
}

// HandlerConfig configures the source-management handler.
type HandlerConfig struct {
	Repo      *SourceRepository
	Audit     AuditWriter
	Validator ConnectorValidator
	Health    HealthReader
	Events    EventEmitter
}

// Handler serves the /v1/admin/sources HTTP API surface.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler validates cfg and returns a Handler. Audit, Validator,
// Health, and Events are optional — when nil they're stubbed with
// no-ops so the handler still works in test setups.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Repo == nil {
		return nil, errors.New("admin: nil Repo")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	if cfg.Validator == nil {
		cfg.Validator = registryValidator{}
	}
	if cfg.Events == nil {
		cfg.Events = noopEvents{}
	}
	return &Handler{cfg: cfg}, nil
}

// Register mounts the admin source endpoints on rg.
//
//	POST   /v1/admin/sources         — connect a new source
//	GET    /v1/admin/sources         — list tenant sources
//	GET    /v1/admin/sources/:id     — fetch a single source
//	PATCH  /v1/admin/sources/:id     — pause / resume / re-scope
//	DELETE /v1/admin/sources/:id     — mark removing (forget worker picks up)
//	GET    /v1/admin/sources/:id/health — sync health
func (h *Handler) Register(rg *gin.RouterGroup) {
	g := rg.Group("/v1/admin/sources")
	g.POST("", h.connect)
	g.GET("", h.list)
	g.GET("/:id", h.get)
	g.PATCH("/:id", h.patch)
	g.DELETE("/:id", h.delete)
	g.GET("/:id/health", h.health)
}

// ConnectRequest is the POST body.
type ConnectRequest struct {
	ConnectorType string         `json:"connector_type"`
	Config        map[string]any `json:"config"`
	Scopes        []string       `json:"scopes"`
	Credentials   []byte         `json:"credentials,omitempty"`
}

// connect serves POST /v1/admin/sources.
func (h *Handler) connect(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req ConnectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.ConnectorType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connector_type required"})
		return
	}

	src := NewSource(tenantID, req.ConnectorType, JSONMap(req.Config), req.Scopes)
	if err := h.cfg.Validator.Validate(c.Request.Context(), req.ConnectorType, connector.ConnectorConfig{
		Name:        req.ConnectorType,
		TenantID:    tenantID,
		SourceID:    src.ID,
		Settings:    map[string]any(req.Config),
		Credentials: req.Credentials,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "validate: " + err.Error()})
		return
	}
	if err := h.cfg.Repo.Create(c.Request.Context(), src); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist: " + err.Error()})
		return
	}

	h.emitAudit(c, src.ID, audit.ActionSourceConnected, audit.JSONMap{
		"connector_type": req.ConnectorType,
		"scopes":         req.Scopes,
	})
	if err := h.cfg.Events.EmitSourceConnected(c.Request.Context(), tenantID, src.ID, req.ConnectorType); err != nil {
		// Non-fatal: the row is persisted; an out-of-band reconciler
		// can retry the kick-off event.
		_ = err
	}

	c.JSON(http.StatusCreated, src)
}

// PatchRequest is the PATCH body. Status accepts "active" / "paused";
// re-scoping is expressed by setting Scopes to a (possibly empty)
// array.
type PatchRequest struct {
	Status *SourceStatus `json:"status,omitempty"`
	Scopes *[]string     `json:"scopes,omitempty"`
}

func (h *Handler) patch(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	var req PatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	patch := UpdatePatch{Status: req.Status, Scopes: req.Scopes}
	updated, err := h.cfg.Repo.Update(c.Request.Context(), tenantID, id, patch)
	if errors.Is(err, ErrSourceNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Status != nil {
		switch *req.Status {
		case SourceStatusPaused:
			h.emitAudit(c, id, audit.ActionSourcePaused, nil)
		case SourceStatusActive:
			h.emitAudit(c, id, audit.ActionSourceResumed, nil)
		}
	}
	if req.Scopes != nil {
		h.emitAudit(c, id, audit.ActionSourceReScoped, audit.JSONMap{"scopes": *req.Scopes})
		// Kick a new sync window; the producer is best-effort, the
		// row is the source of truth.
		_ = h.cfg.Events.EmitSourceConnected(c.Request.Context(), tenantID, id, updated.ConnectorType)
	}

	c.JSON(http.StatusOK, updated)
}

func (h *Handler) delete(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}

	updated, err := h.cfg.Repo.MarkRemoving(c.Request.Context(), tenantID, id)
	if errors.Is(err, ErrSourceNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// The audit event for the actual purge fires from the forget worker
	// (source.purged); here we just record the admin's intent.
	h.emitAudit(c, id, audit.ActionSourcePurged, audit.JSONMap{"intent": true})
	c.JSON(http.StatusAccepted, updated)
}

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	out, err := h.cfg.Repo.List(c.Request.Context(), ListFilter{
		TenantID: tenantID,
		Status:   SourceStatus(c.Query("status")),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *Handler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	src, err := h.cfg.Repo.Get(c.Request.Context(), tenantID, id)
	if errors.Is(err, ErrSourceNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, src)
}

func (h *Handler) health(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if h.cfg.Health == nil {
		c.JSON(http.StatusOK, gin.H{"status": HealthStatusUnknown})
		return
	}
	hh, err := h.cfg.Health.Get(c.Request.Context(), tenantID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if hh == nil {
		c.JSON(http.StatusOK, gin.H{"status": HealthStatusUnknown})
		return
	}
	c.JSON(http.StatusOK, hh)
}

// emitAudit records an admin action, swallowing errors — a missing
// audit row must not fail the user-facing operation.
func (h *Handler) emitAudit(c *gin.Context, sourceID string, action audit.Action, meta audit.JSONMap) {
	tenantID, _ := tenantIDFromContext(c)
	actorID := actorIDFromContext(c)
	log := audit.NewAuditLog(tenantID, actorID, action, "source", sourceID, meta, "")
	_ = h.cfg.Audit.Create(c.Request.Context(), log)
}

// tenantIDFromContext mirrors the audit-handler implementation.
func tenantIDFromContext(c *gin.Context) (string, bool) {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func actorIDFromContext(c *gin.Context) string {
	v, ok := c.Get(audit.ActorContextKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// noopAudit is the default AuditWriter when none is wired.
type noopAudit struct{}

func (noopAudit) Create(_ context.Context, _ *audit.AuditLog) error { return nil }

// noopEvents is the default EventEmitter when none is wired.
type noopEvents struct{}

func (noopEvents) EmitSourceConnected(_ context.Context, _, _, _ string) error { return nil }

// registryValidator is the default ConnectorValidator. It looks the
// connector up in the package-global registry and calls the
// instance's Validate.
type registryValidator struct{}

func (registryValidator) Validate(ctx context.Context, connectorType string, cfg connector.ConnectorConfig) error {
	factory, err := connector.GetSourceConnector(connectorType)
	if err != nil {
		return err
	}
	return factory().Validate(ctx, cfg)
}

// Marshal is exported so tests can assert the JSON shape of a Source.
func Marshal(src *Source) ([]byte, error) { return json.Marshal(src) }
