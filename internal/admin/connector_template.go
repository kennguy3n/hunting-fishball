package admin

// connector_template.go — Round-6 Task 16.
//
// Connector templates pre-populate config skeletons so operators
// can spin up new sources without re-deriving the JSON shape every
// time. Templates are tenant-scoped (so each tenant can keep their
// own org-wide presets) and the source-creation handler accepts an
// optional `template_id` field that merges the template's
// default_config into the new source's config (request-supplied
// keys win).

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// ConnectorTemplate is the persisted template shape.
type ConnectorTemplate struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	ConnectorType string    `json:"connector_type"`
	DefaultConfig JSONMap   `json:"default_config"`
	Description   string    `json:"description,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Validate returns nil when t is well-formed.
func (t *ConnectorTemplate) Validate() error {
	if t == nil {
		return errors.New("template: nil")
	}
	if t.TenantID == "" {
		return errors.New("template: missing tenant_id")
	}
	if t.ConnectorType == "" {
		return errors.New("template: missing connector_type")
	}
	return nil
}

// ConnectorTemplateStore is the persistence port.
type ConnectorTemplateStore interface {
	List(tenantID string) ([]*ConnectorTemplate, error)
	Get(tenantID, id string) (*ConnectorTemplate, error)
	Create(*ConnectorTemplate) error
	Delete(tenantID, id string) error
}

// InMemoryConnectorTemplateStore is a goroutine-safe map-backed
// implementation used in tests.
type InMemoryConnectorTemplateStore struct {
	mu   sync.RWMutex
	rows map[string]*ConnectorTemplate
	seq  int64
}

// NewInMemoryConnectorTemplateStore constructs the in-memory
// store.
func NewInMemoryConnectorTemplateStore() *InMemoryConnectorTemplateStore {
	return &InMemoryConnectorTemplateStore{rows: map[string]*ConnectorTemplate{}}
}

// List implements ConnectorTemplateStore.
func (s *InMemoryConnectorTemplateStore) List(tenantID string) ([]*ConnectorTemplate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*ConnectorTemplate{}
	for _, r := range s.rows {
		if r.TenantID == tenantID {
			out = append(out, r)
		}
	}
	return out, nil
}

// Get implements ConnectorTemplateStore.
func (s *InMemoryConnectorTemplateStore) Get(tenantID, id string) (*ConnectorTemplate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, errors.New("template: not found")
	}
	cp := *r
	return &cp, nil
}

// Create implements ConnectorTemplateStore.
func (s *InMemoryConnectorTemplateStore) Create(t *ConnectorTemplate) error {
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		s.seq++
		t.ID = fmt.Sprintf("ctmpl-%d", s.seq)
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	cp := *t
	s.rows[t.ID] = &cp
	return nil
}

// Delete implements ConnectorTemplateStore.
func (s *InMemoryConnectorTemplateStore) Delete(tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.TenantID != tenantID {
		return errors.New("template: not found")
	}
	delete(s.rows, id)
	return nil
}

// ApplyTemplate merges template defaults into reqConfig. Keys
// already set in reqConfig win (operators can override). Returns a
// new map; inputs are unchanged.
func ApplyTemplate(template *ConnectorTemplate, reqConfig JSONMap) JSONMap {
	out := JSONMap{}
	if template != nil {
		for k, v := range template.DefaultConfig {
			out[k] = v
		}
	}
	for k, v := range reqConfig {
		out[k] = v
	}
	return out
}

// ConnectorTemplateHandler exposes the admin CRUD surface.
type ConnectorTemplateHandler struct {
	store ConnectorTemplateStore
	audit AuditWriter
}

// NewConnectorTemplateHandler validates and constructs the handler.
func NewConnectorTemplateHandler(store ConnectorTemplateStore, auditWriter AuditWriter) (*ConnectorTemplateHandler, error) {
	if store == nil {
		return nil, errors.New("template handler: nil store")
	}
	return &ConnectorTemplateHandler{store: store, audit: auditWriter}, nil
}

// Register attaches routes.
func (h *ConnectorTemplateHandler) Register(g *gin.RouterGroup) {
	g.GET("/v1/admin/connector-templates", h.list)
	g.POST("/v1/admin/connector-templates", h.create)
	g.GET("/v1/admin/connector-templates/:id", h.get)
	g.DELETE("/v1/admin/connector-templates/:id", h.delete)
}

func (h *ConnectorTemplateHandler) tenant(c *gin.Context) (string, bool) {
	tID, _ := c.Get(audit.TenantContextKey)
	tenantID, _ := tID.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return "", false
	}
	return tenantID, true
}

func (h *ConnectorTemplateHandler) list(c *gin.Context) {
	tenantID, ok := h.tenant(c)
	if !ok {
		return
	}
	rows, err := h.store.List(tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"templates": rows})
}

func (h *ConnectorTemplateHandler) create(c *gin.Context) {
	tenantID, ok := h.tenant(c)
	if !ok {
		return
	}
	var body ConnectorTemplate
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	body.TenantID = tenantID
	if err := h.store.Create(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, body)
}

func (h *ConnectorTemplateHandler) get(c *gin.Context) {
	tenantID, ok := h.tenant(c)
	if !ok {
		return
	}
	t, err := h.store.Get(tenantID, c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, t)
}

func (h *ConnectorTemplateHandler) delete(c *gin.Context) {
	tenantID, ok := h.tenant(c)
	if !ok {
		return
	}
	if err := h.store.Delete(tenantID, c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
