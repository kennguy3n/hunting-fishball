package admin

// schema_handler.go — Round-6 Task 8.
//
// GET /v1/admin/sources/:id/schema returns the namespaces the
// configured connector exposes (drives, channels, sites, …) plus
// any per-namespace metadata. The handler is the first half of the
// "source onboarding" flow: operators preview what they would
// ingest BEFORE flipping `Status=active`.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// SchemaResolver is the narrow port the handler depends on.
// Default wiring uses ConnectorRegistryResolver (below) which
// looks the connector up via the global registry.
type SchemaResolver interface {
	Resolve(ctx context.Context, src *Source) ([]connector.Namespace, error)
}

// SchemaSourceLookup is the read-side tenant-scoped lookup.
type SchemaSourceLookup interface {
	GetByIDForTenant(ctx context.Context, tenantID, sourceID string) (*Source, error)
}

// SchemaHandlerConfig wires the schema endpoint.
type SchemaHandlerConfig struct {
	Lookup   SchemaSourceLookup
	Resolver SchemaResolver
	Audit    AuditWriter
}

// SchemaHandler exposes GET /v1/admin/sources/:id/schema.
type SchemaHandler struct {
	cfg SchemaHandlerConfig
}

// NewSchemaHandler constructs a SchemaHandler.
func NewSchemaHandler(cfg SchemaHandlerConfig) (*SchemaHandler, error) {
	if cfg.Lookup == nil {
		return nil, errors.New("schema handler: nil Lookup")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("schema handler: nil Resolver")
	}
	return &SchemaHandler{cfg: cfg}, nil
}

// Register attaches the schema route to g.
func (h *SchemaHandler) Register(g *gin.RouterGroup) {
	g.GET("/v1/admin/sources/:id/schema", h.get)
}

// SchemaResponse is the JSON shape returned by the endpoint.
type SchemaResponse struct {
	SourceID      string                  `json:"source_id"`
	ConnectorType string                  `json:"connector_type"`
	Namespaces    []SchemaNamespaceDetail `json:"namespaces"`
}

// SchemaNamespaceDetail mirrors connector.Namespace for the JSON
// wire format; we keep a separate type so the public API doesn't
// leak internal connector struct evolution.
type SchemaNamespaceDetail struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (h *SchemaHandler) get(c *gin.Context) {
	tenantID, _ := c.Get(audit.TenantContextKey)
	tID, _ := tenantID.(string)
	if tID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing source id"})
		return
	}
	src, err := h.cfg.Lookup.GetByIDForTenant(c.Request.Context(), tID, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if src == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	nss, err := h.cfg.Resolver.Resolve(c.Request.Context(), src)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("resolve schema: %v", err)})
		return
	}
	out := make([]SchemaNamespaceDetail, 0, len(nss))
	for _, ns := range nss {
		out = append(out, SchemaNamespaceDetail{
			ID: ns.ID, Name: ns.Name, Kind: ns.Kind, Metadata: ns.Metadata,
		})
	}
	c.JSON(http.StatusOK, SchemaResponse{
		SourceID: src.ID, ConnectorType: src.ConnectorType, Namespaces: out,
	})
}

// ConnectorRegistryResolver is the production SchemaResolver. It
// looks the connector up via the global registry, builds a
// connection from the source config, and calls ListNamespaces.
type ConnectorRegistryResolver struct{}

// Resolve implements SchemaResolver.
func (ConnectorRegistryResolver) Resolve(ctx context.Context, src *Source) ([]connector.Namespace, error) {
	if src == nil {
		return nil, errors.New("nil source")
	}
	factory, err := connector.GetSourceConnector(src.ConnectorType)
	if err != nil {
		return nil, fmt.Errorf("connector lookup: %w", err)
	}
	cn := factory()
	if cn == nil {
		return nil, errors.New("connector instantiate: nil")
	}
	cfg := connector.ConnectorConfig{
		Name:     src.ConnectorType,
		TenantID: src.TenantID,
		SourceID: src.ID,
		Settings: map[string]any(src.Config),
	}
	if err := cn.Validate(ctx, cfg); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	conn, err := cn.Connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = cn.Disconnect(ctx, conn) }()
	return cn.ListNamespaces(ctx, conn)
}
