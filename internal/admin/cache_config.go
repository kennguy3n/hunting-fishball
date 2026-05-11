// cache_config.go — Round-7 Task 15.
//
// Per-tenant semantic-cache TTL. The retrieval handler consults
// the store when writing a cache entry and falls back to the
// global default when no row exists for the tenant.
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

// CacheConfig is the persisted row.
type CacheConfig struct {
	TenantID  string    `gorm:"column:tenant_id;type:char(26);primaryKey" json:"tenant_id"`
	TTLMS     int       `gorm:"column:ttl_ms;not null;default:0" json:"ttl_ms"`
	Notes     string    `gorm:"column:notes;not null;default:''" json:"notes"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName overrides GORM pluralization.
func (CacheConfig) TableName() string { return "tenant_cache_config" }

// CacheTTLStore is the CRUD port.
type CacheTTLStore interface {
	Get(ctx context.Context, tenantID string) (*CacheConfig, error)
	Put(ctx context.Context, c *CacheConfig) error
	// TTLFor returns the per-tenant TTL or the supplied fallback
	// if the tenant has no override.
	TTLFor(ctx context.Context, tenantID string, fallback time.Duration) time.Duration
}

// ErrCacheConfigNotFound is returned by Get when no row exists.
var ErrCacheConfigNotFound = errors.New("cache_config: not found")

// CacheTTLGORM is the Postgres-backed store.
type CacheTTLGORM struct{ db *gorm.DB }

// NewCacheTTLGORM validates inputs.
func NewCacheTTLGORM(db *gorm.DB) (*CacheTTLGORM, error) {
	if db == nil {
		return nil, errors.New("cache_config: nil db")
	}
	return &CacheTTLGORM{db: db}, nil
}

// Get returns the row or ErrCacheConfigNotFound.
func (s *CacheTTLGORM) Get(ctx context.Context, tenantID string) (*CacheConfig, error) {
	var c CacheConfig
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrCacheConfigNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Put upserts the row.
func (s *CacheTTLGORM) Put(ctx context.Context, c *CacheConfig) error {
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	return s.db.WithContext(ctx).Save(c).Error
}

// TTLFor returns the per-tenant TTL or fallback.
func (s *CacheTTLGORM) TTLFor(ctx context.Context, tenantID string, fallback time.Duration) time.Duration {
	c, err := s.Get(ctx, tenantID)
	if err != nil || c.TTLMS <= 0 {
		return fallback
	}
	return time.Duration(c.TTLMS) * time.Millisecond
}

// InMemoryCacheTTLStore is the test/standalone fake.
type InMemoryCacheTTLStore struct {
	mu   sync.RWMutex
	rows map[string]*CacheConfig
}

// NewInMemoryCacheTTLStore returns the fake.
func NewInMemoryCacheTTLStore() *InMemoryCacheTTLStore {
	return &InMemoryCacheTTLStore{rows: map[string]*CacheConfig{}}
}

// Get returns a copy of the row.
func (s *InMemoryCacheTTLStore) Get(_ context.Context, tenantID string) (*CacheConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[tenantID]
	if !ok {
		return nil, ErrCacheConfigNotFound
	}
	cp := *row
	return &cp, nil
}

// Put stores a copy of the row.
func (s *InMemoryCacheTTLStore) Put(_ context.Context, c *CacheConfig) error {
	if c == nil || c.TenantID == "" {
		return errors.New("cache_config: missing tenant_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	cp := *c
	s.rows[c.TenantID] = &cp
	return nil
}

// TTLFor returns the per-tenant TTL or fallback.
func (s *InMemoryCacheTTLStore) TTLFor(_ context.Context, tenantID string, fallback time.Duration) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row, ok := s.rows[tenantID]
	if !ok || row.TTLMS <= 0 {
		return fallback
	}
	return time.Duration(row.TTLMS) * time.Millisecond
}

// CacheConfigHandler exposes GET/PUT
// /v1/admin/tenants/:id/cache-config.
type CacheConfigHandler struct {
	store CacheTTLStore
	audit AuditWriter
}

// NewCacheConfigHandler validates inputs.
func NewCacheConfigHandler(store CacheTTLStore, aw AuditWriter) (*CacheConfigHandler, error) {
	if store == nil {
		return nil, errors.New("cache_config: nil store")
	}
	if aw == nil {
		aw = noopAudit{}
	}
	return &CacheConfigHandler{store: store, audit: aw}, nil
}

// Register mounts the routes.
func (h *CacheConfigHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/tenants/:id/cache-config", h.get)
	rg.PUT("/v1/admin/tenants/:id/cache-config", h.put)
}

func (h *CacheConfigHandler) get(c *gin.Context) {
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
	if errors.Is(err, ErrCacheConfigNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}

func (h *CacheConfigHandler) put(c *gin.Context) {
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
	var body CacheConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.TTLMS < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ttl_ms must be >= 0"})
		return
	}
	body.TenantID = id
	if err := h.store.Put(c.Request.Context(), &body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actor := actorIDFromContext(c)
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		id, actor, audit.ActionCacheConfigUpdated, "tenant", id,
		audit.JSONMap{"ttl_ms": body.TTLMS}, "",
	))
	c.JSON(http.StatusOK, &body)
}
