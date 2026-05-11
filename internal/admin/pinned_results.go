// pinned_results.go — Round-7 Task 17.
//
// Operator-pinned retrieval results. The store is owned by the
// admin package; the retrieval package consumes the read-only
// `Lookup` interface to inject pins into its merged hit list.
// Splitting the responsibilities this way avoids an import
// cycle through the existing retrieval → admin link
// (abtest_router_adapter).
package admin

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// PinnedResult is a single (chunk_id, position) entry.
type PinnedResult struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	QueryPattern string    `json:"query_pattern"`
	ChunkID      string    `json:"chunk_id"`
	Position     int       `json:"position"`
	CreatedAt    time.Time `json:"created_at"`
}

// PinnedResultStore is the CRUD port.
type PinnedResultStore interface {
	Lookup(ctx context.Context, tenantID, query string) ([]PinnedResult, error)
	List(ctx context.Context, tenantID string) ([]PinnedResult, error)
	Create(ctx context.Context, p PinnedResult) (PinnedResult, error)
	Delete(ctx context.Context, tenantID, id string) error
}

// InMemoryPinnedResultStore is the test/standalone fake.
type InMemoryPinnedResultStore struct {
	mu   sync.RWMutex
	rows []PinnedResult
	idFn func() string
}

// NewInMemoryPinnedResultStore returns the fake. The optional
// idFn lets tests inject deterministic IDs.
func NewInMemoryPinnedResultStore(idFn func() string) *InMemoryPinnedResultStore {
	if idFn == nil {
		idFn = newPinID
	}
	return &InMemoryPinnedResultStore{idFn: idFn}
}

// Lookup returns pins whose `QueryPattern` matches `query` exactly.
func (s *InMemoryPinnedResultStore) Lookup(_ context.Context, tenantID, query string) ([]PinnedResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []PinnedResult
	for _, r := range s.rows {
		if r.TenantID == tenantID && r.QueryPattern == query {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, nil
}

// List returns every pin for the tenant.
func (s *InMemoryPinnedResultStore) List(_ context.Context, tenantID string) ([]PinnedResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []PinnedResult
	for _, r := range s.rows {
		if r.TenantID == tenantID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].QueryPattern == out[j].QueryPattern {
			return out[i].Position < out[j].Position
		}
		return out[i].QueryPattern < out[j].QueryPattern
	})
	return out, nil
}

// Create inserts a new pin, assigning an ID and CreatedAt.
func (s *InMemoryPinnedResultStore) Create(_ context.Context, p PinnedResult) (PinnedResult, error) {
	if p.TenantID == "" || p.QueryPattern == "" || p.ChunkID == "" {
		return PinnedResult{}, errors.New("pinned_results: missing required field")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = s.idFn()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	s.rows = append(s.rows, p)
	return p, nil
}

// Delete removes the pin by ID, scoped to tenant.
func (s *InMemoryPinnedResultStore) Delete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rows {
		if r.TenantID == tenantID && r.ID == id {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return errors.New("pinned_results: not found")
}

// PinnedResultsHandler is the HTTP surface.
type PinnedResultsHandler struct {
	store PinnedResultStore
	audit AuditWriter
}

// NewPinnedResultsHandler validates inputs.
func NewPinnedResultsHandler(store PinnedResultStore, aw AuditWriter) (*PinnedResultsHandler, error) {
	if store == nil {
		return nil, errors.New("pinned_results: nil store")
	}
	if aw == nil {
		aw = noopAudit{}
	}
	return &PinnedResultsHandler{store: store, audit: aw}, nil
}

// Register mounts the three routes.
func (h *PinnedResultsHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/retrieval/pins", h.create)
	rg.GET("/v1/admin/retrieval/pins", h.list)
	rg.DELETE("/v1/admin/retrieval/pins/:id", h.delete)
}

// PinnedResultRequest is the create body.
type PinnedResultRequest struct {
	QueryPattern string `json:"query_pattern" binding:"required"`
	ChunkID      string `json:"chunk_id" binding:"required"`
	Position     int    `json:"position"`
}

func (h *PinnedResultsHandler) create(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var body PinnedResultRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Position < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "position must be >= 0"})
		return
	}
	pin, err := h.store.Create(c.Request.Context(), PinnedResult{
		TenantID: tenantID, QueryPattern: body.QueryPattern,
		ChunkID: body.ChunkID, Position: body.Position,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	actor := actorIDFromContext(c)
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actor, audit.ActionRetrievalPinned, "retrieval", pin.ID,
		audit.JSONMap{"query_pattern": pin.QueryPattern, "chunk_id": pin.ChunkID, "position": pin.Position}, "",
	))
	c.JSON(http.StatusCreated, pin)
}

func (h *PinnedResultsHandler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	rows, err := h.store.List(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pins": rows})
}

func (h *PinnedResultsHandler) delete(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	if err := h.store.Delete(c.Request.Context(), tenantID, c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// newPinID is a tiny ID generator used when callers don't inject
// their own (e.g. tests).
func newPinID() string {
	const alphabet = "0123456789abcdef"
	now := time.Now().UnixNano()
	b := make([]byte, 16)
	for i := 0; i < 16; i++ {
		b[i] = alphabet[(now>>uint(i*4))&0xF]
	}
	return string(b)
}
