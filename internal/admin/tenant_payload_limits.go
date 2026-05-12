// tenant_payload_limits.go — Round-14 Task 8.
//
// Per-tenant override of the global request-body cap (Round-13
// Task 11). The GORM store reads / writes the
// tenant_payload_limits table seeded by migration 039.
//
// The Gin layer plugs the store in via the new
// PayloadLimiterConfig.TenantOverride hook: when the row exists,
// its max_bytes substitutes for the global default on a single
// request; when it does not exist, the global cap still applies.
//
// The cache is intentionally thin (process-local, no TTL): the
// override is admin-set and rarely changes, but we keep a
// 30-second invalidation window so an operator can lower the cap
// without bouncing the API server.
package admin

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// TenantPayloadLimitRow is the GORM mapping for the
// tenant_payload_limits table.
type TenantPayloadLimitRow struct {
	TenantID  string    `gorm:"type:char(26);primaryKey;column:tenant_id"`
	MaxBytes  int64     `gorm:"not null;column:max_bytes"`
	UpdatedAt time.Time `gorm:"not null;default:now();column:updated_at"`
}

// TableName pins the table name independently of GORM's plural
// inflection.
func (TenantPayloadLimitRow) TableName() string { return "tenant_payload_limits" }

// TenantPayloadLimitStore is the read seam.
type TenantPayloadLimitStore interface {
	Get(ctx context.Context, tenantID string) (int64, bool, error)
	Upsert(ctx context.Context, tenantID string, maxBytes int64) error
	Delete(ctx context.Context, tenantID string) error
}

// TenantPayloadLimitStoreGORM is the Postgres-backed
// implementation.
type TenantPayloadLimitStoreGORM struct {
	db *gorm.DB
}

// NewTenantPayloadLimitStoreGORM wires the store.
func NewTenantPayloadLimitStoreGORM(db *gorm.DB) *TenantPayloadLimitStoreGORM {
	return &TenantPayloadLimitStoreGORM{db: db}
}

// Get returns the override for tenantID or (0, false, nil) when
// no row exists.
func (s *TenantPayloadLimitStoreGORM) Get(ctx context.Context, tenantID string) (int64, bool, error) {
	if tenantID == "" {
		return 0, false, errors.New("tenant_payload_limits: tenant_id required")
	}
	var row TenantPayloadLimitRow
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return row.MaxBytes, true, nil
}

// Upsert writes maxBytes for tenantID.
func (s *TenantPayloadLimitStoreGORM) Upsert(ctx context.Context, tenantID string, maxBytes int64) error {
	if tenantID == "" {
		return errors.New("tenant_payload_limits: tenant_id required")
	}
	if maxBytes <= 0 {
		return errors.New("tenant_payload_limits: max_bytes must be > 0")
	}
	row := TenantPayloadLimitRow{TenantID: tenantID, MaxBytes: maxBytes, UpdatedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Save(&row).Error
}

// Delete drops the override row, reverting the tenant to the
// global cap.
func (s *TenantPayloadLimitStoreGORM) Delete(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return errors.New("tenant_payload_limits: tenant_id required")
	}
	return s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Delete(&TenantPayloadLimitRow{}).Error
}

// InMemoryTenantPayloadLimitStore is the test-friendly fake.
type InMemoryTenantPayloadLimitStore struct {
	mu   sync.RWMutex
	data map[string]int64
}

// NewInMemoryTenantPayloadLimitStore constructs an empty store.
func NewInMemoryTenantPayloadLimitStore() *InMemoryTenantPayloadLimitStore {
	return &InMemoryTenantPayloadLimitStore{data: map[string]int64{}}
}

// Get returns the override.
func (s *InMemoryTenantPayloadLimitStore) Get(_ context.Context, tenantID string) (int64, bool, error) {
	if tenantID == "" {
		return 0, false, errors.New("tenant_payload_limits: tenant_id required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[tenantID]
	return v, ok, nil
}

// Upsert writes the override.
func (s *InMemoryTenantPayloadLimitStore) Upsert(_ context.Context, tenantID string, maxBytes int64) error {
	if tenantID == "" {
		return errors.New("tenant_payload_limits: tenant_id required")
	}
	if maxBytes <= 0 {
		return errors.New("tenant_payload_limits: max_bytes must be > 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[tenantID] = maxBytes
	return nil
}

// Delete removes the override.
func (s *InMemoryTenantPayloadLimitStore) Delete(_ context.Context, tenantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, tenantID)
	return nil
}

// TenantPayloadLookup adapts a TenantPayloadLimitStore to the
// PayloadLimiterConfig.TenantOverride signature. The lookup uses
// the audit context key set by the auth middleware: when the key
// is missing the override is skipped and the global cap applies.
// A 30-second process-local cache keeps the hot path off the DB
// for steady-state traffic.
func TenantPayloadLookup(store TenantPayloadLimitStore) func(c *gin.Context) (int64, bool) {
	cache := newTenantLimitCache(30 * time.Second)
	return func(c *gin.Context) (int64, bool) {
		if store == nil {
			return 0, false
		}
		val, _ := c.Get(audit.TenantContextKey)
		tenantID, _ := val.(string)
		if tenantID == "" {
			return 0, false
		}
		if cap, ok := cache.get(tenantID); ok {
			if cap == 0 {
				return 0, false
			}
			return cap, true
		}
		cap, ok, err := store.Get(c.Request.Context(), tenantID)
		if err != nil {
			return 0, false
		}
		if !ok {
			cache.put(tenantID, 0)
			return 0, false
		}
		cache.put(tenantID, cap)
		return cap, true
	}
}

type tenantLimitCacheEntry struct {
	cap       int64
	expiresAt time.Time
}

type tenantLimitCache struct {
	mu  sync.RWMutex
	ttl time.Duration
	m   map[string]tenantLimitCacheEntry
}

func newTenantLimitCache(ttl time.Duration) *tenantLimitCache {
	return &tenantLimitCache{ttl: ttl, m: map[string]tenantLimitCacheEntry{}}
}

func (c *tenantLimitCache) get(tenantID string) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[tenantID]
	if !ok {
		return 0, false
	}
	if time.Now().After(e.expiresAt) {
		return 0, false
	}
	return e.cap, true
}

func (c *tenantLimitCache) put(tenantID string, cap int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[tenantID] = tenantLimitCacheEntry{cap: cap, expiresAt: time.Now().Add(c.ttl)}
}
