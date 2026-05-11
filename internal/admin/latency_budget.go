// latency_budget.go — Round-7 Task 11.
//
// Per-tenant retrieval latency budgets. The retrieval handler
// consults the LatencyBudgetStore when no explicit
// max_latency_ms is set on the request, and emits the
// `context_engine_retrieval_budget_violations_total{tenant_id}`
// counter (see internal/observability.ObserveBudgetViolation)
// when the observed latency exceeds the budget.
package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// LatencyBudget is the GORM model and JSON wire shape.
type LatencyBudget struct {
	TenantID     string    `gorm:"type:char(26);primaryKey;column:tenant_id" json:"tenant_id"`
	MaxLatencyMS int       `gorm:"column:max_latency_ms;not null;default:500" json:"max_latency_ms"`
	P95TargetMS  int       `gorm:"column:p95_target_ms;not null;default:500" json:"p95_target_ms"`
	Notes        string    `gorm:"column:notes;not null;default:''" json:"notes"`
	CreatedAt    time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName overrides the default GORM pluralization.
func (LatencyBudget) TableName() string { return "tenant_latency_budgets" }

// LatencyBudgetStore is the CRUD port.
type LatencyBudgetStore interface {
	Get(ctx context.Context, tenantID string) (*LatencyBudget, error)
	Put(ctx context.Context, b *LatencyBudget) error
}

// LatencyBudgetGORM is the Postgres implementation.
type LatencyBudgetGORM struct{ db *gorm.DB }

// NewLatencyBudgetGORM wires the store.
func NewLatencyBudgetGORM(db *gorm.DB) (*LatencyBudgetGORM, error) {
	if db == nil {
		return nil, errors.New("latency_budget: nil db")
	}
	return &LatencyBudgetGORM{db: db}, nil
}

// Get returns the row or ErrLatencyBudgetNotFound.
func (s *LatencyBudgetGORM) Get(ctx context.Context, tenantID string) (*LatencyBudget, error) {
	var b LatencyBudget
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLatencyBudgetNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Put upserts the row.
func (s *LatencyBudgetGORM) Put(ctx context.Context, b *LatencyBudget) error {
	now := time.Now().UTC()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	return s.db.WithContext(ctx).Save(b).Error
}

// ErrLatencyBudgetNotFound is returned when Get cannot find a row.
var ErrLatencyBudgetNotFound = errors.New("latency_budget: not found")

// InMemoryLatencyBudgetStore is a goroutine-safe map-backed fake
// used by tests and standalone deployments without Postgres.
type InMemoryLatencyBudgetStore struct {
	mu   sync.RWMutex
	rows map[string]*LatencyBudget
}

// NewInMemoryLatencyBudgetStore constructs the fake.
func NewInMemoryLatencyBudgetStore() *InMemoryLatencyBudgetStore {
	return &InMemoryLatencyBudgetStore{rows: map[string]*LatencyBudget{}}
}

// Get returns a copy of the row or ErrLatencyBudgetNotFound.
func (s *InMemoryLatencyBudgetStore) Get(_ context.Context, tenantID string) (*LatencyBudget, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[tenantID]
	if !ok {
		return nil, ErrLatencyBudgetNotFound
	}
	cp := *row
	return &cp, nil
}

// Put stores a copy of the row.
func (s *InMemoryLatencyBudgetStore) Put(_ context.Context, b *LatencyBudget) error {
	if b == nil || b.TenantID == "" {
		return errors.New("latency_budget: missing tenant_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	cp := *b
	s.rows[b.TenantID] = &cp
	return nil
}

// LatencyBudgetHandler exposes GET/PUT
// /v1/admin/tenants/:id/latency-budget.
type LatencyBudgetHandler struct {
	store LatencyBudgetStore
	audit AuditWriter
}

// NewLatencyBudgetHandler validates inputs.
func NewLatencyBudgetHandler(store LatencyBudgetStore, aw AuditWriter) (*LatencyBudgetHandler, error) {
	if store == nil {
		return nil, errors.New("latency_budget: nil store")
	}
	if aw == nil {
		aw = noopAudit{}
	}
	return &LatencyBudgetHandler{store: store, audit: aw}, nil
}

// Register mounts the routes.
func (h *LatencyBudgetHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/tenants/:id/latency-budget", h.get)
	rg.PUT("/v1/admin/tenants/:id/latency-budget", h.put)
}

func (h *LatencyBudgetHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id != tenantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-tenant access denied"})
		return
	}
	row, err := h.store.Get(c.Request.Context(), id)
	if errors.Is(err, ErrLatencyBudgetNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}

func (h *LatencyBudgetHandler) put(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id != tenantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-tenant access denied"})
		return
	}
	var body LatencyBudget
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.MaxLatencyMS <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_latency_ms must be positive"})
		return
	}
	body.TenantID = id
	if err := h.store.Put(c.Request.Context(), &body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actor := actorIDFromContext(c)
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		id, actor, audit.ActionRetrievalBudgetUpdated, "tenant", id,
		audit.JSONMap{"max_latency_ms": body.MaxLatencyMS, "p95_target_ms": body.P95TargetMS}, "",
	))
	c.JSON(http.StatusOK, &body)
}
