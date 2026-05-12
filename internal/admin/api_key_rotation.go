// api_key_rotation.go — Round-13 Task 10.
//
// POST /v1/admin/tenants/:tenant_id/rotate-api-key generates a new
// API key for the tenant and returns it once. The previously
// active key is moved to the "grace" state for a configurable
// window (CONTEXT_ENGINE_API_KEY_GRACE_PERIOD, default 24h) so
// clients have time to update before being rejected.
//
// The persisted shape (api_keys table from
// migrations/037_api_keys.sql) stores the SHA-256 hash of the
// generated key rather than the raw value so a future database
// breach cannot leak live credentials. The cleartext key is
// returned exactly once in the HTTP response.
//
// The handler emits an `api_key.rotated` audit event so operators
// can correlate rotations with downstream credential issues.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// APIKeyStatus enumerates the three states a row can be in.
type APIKeyStatus string

const (
	// APIKeyStatusActive is the freshly issued key. Accepted on
	// every request.
	APIKeyStatusActive APIKeyStatus = "active"
	// APIKeyStatusGrace is the previously active key that has
	// been superseded. Still accepted while now() <= grace_until.
	APIKeyStatusGrace APIKeyStatus = "grace"
	// APIKeyStatusRevoked is unconditionally rejected.
	APIKeyStatusRevoked APIKeyStatus = "revoked"
)

// APIKeyRow is the persisted shape. GORM column tags map to the
// migration in migrations/037_api_keys.sql.
type APIKeyRow struct {
	ID            string     `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID      string     `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	KeyHash       string     `gorm:"type:varchar(64);not null;uniqueIndex;column:key_hash" json:"-"`
	Status        string     `gorm:"type:varchar(16);not null;default:'active';column:status" json:"status"`
	CreatedAt     time.Time  `gorm:"not null;column:created_at" json:"created_at"`
	DeactivatedAt *time.Time `gorm:"column:deactivated_at" json:"deactivated_at,omitempty"`
	GraceUntil    *time.Time `gorm:"column:grace_until" json:"grace_until,omitempty"`
}

// TableName pins the GORM table name.
func (APIKeyRow) TableName() string { return "api_keys" }

// APIKeyStore is the persistence port.
//
// Rotate atomically moves every active row for the tenant to
// `grace` and inserts the supplied new active row in a single
// transaction. Callers should prefer Rotate over chained
// MoveActiveToGrace + Insert: if the insert fails after the
// grace flip commits, the tenant loses all active keys with no
// replacement.
type APIKeyStore interface {
	Insert(ctx context.Context, row *APIKeyRow) error
	MoveActiveToGrace(ctx context.Context, tenantID string, graceUntil time.Time) error
	Rotate(ctx context.Context, tenantID string, graceUntil time.Time, newRow *APIKeyRow) error
}

// APIKeyStoreGORM is the Postgres-backed store.
type APIKeyStoreGORM struct{ db *gorm.DB }

// NewAPIKeyStoreGORM constructs the store.
func NewAPIKeyStoreGORM(db *gorm.DB) *APIKeyStoreGORM {
	return &APIKeyStoreGORM{db: db}
}

// AutoMigrate runs the GORM migration. Real deployments use the
// SQL migration; this is for tests.
func (s *APIKeyStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&APIKeyRow{})
}

// Insert persists a freshly minted key row.
func (s *APIKeyStoreGORM) Insert(ctx context.Context, row *APIKeyRow) error {
	if row.TenantID == "" || row.KeyHash == "" {
		return errors.New("api_keys: missing tenant_id or key_hash")
	}
	return s.db.WithContext(ctx).Create(row).Error
}

// MoveActiveToGrace updates every active row for the tenant to
// `grace` with the supplied grace_until.
func (s *APIKeyStoreGORM) MoveActiveToGrace(ctx context.Context, tenantID string, graceUntil time.Time) error {
	if tenantID == "" {
		return errors.New("api_keys: missing tenant_id")
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).
		Model(&APIKeyRow{}).
		Where("tenant_id = ? AND status = ?", tenantID, string(APIKeyStatusActive)).
		Updates(map[string]any{
			"status":         string(APIKeyStatusGrace),
			"deactivated_at": &now,
			"grace_until":    &graceUntil,
		}).Error
}

// Rotate runs MoveActiveToGrace + Insert in a single transaction
// so a tenant never observes a window with zero active keys (or,
// worse, every key flipped to `grace` with no replacement) when
// the insert leg fails.
func (s *APIKeyStoreGORM) Rotate(ctx context.Context, tenantID string, graceUntil time.Time, newRow *APIKeyRow) error {
	if tenantID == "" {
		return errors.New("api_keys: missing tenant_id")
	}
	if newRow == nil || newRow.TenantID == "" || newRow.KeyHash == "" {
		return errors.New("api_keys: missing tenant_id or key_hash on new row")
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&APIKeyRow{}).
			Where("tenant_id = ? AND status = ?", tenantID, string(APIKeyStatusActive)).
			Updates(map[string]any{
				"status":         string(APIKeyStatusGrace),
				"deactivated_at": &now,
				"grace_until":    &graceUntil,
			}).Error; err != nil {
			return err
		}
		return tx.Create(newRow).Error
	})
}

// APIKeyRotationHandler serves the rotation endpoint.
type APIKeyRotationHandler struct {
	store APIKeyStore
	audit AuditRecorder
	now   func() time.Time
}

// AuditRecorder is the minimal recorder shape the handler needs.
// Method matches audit.Repository.Create so the production wiring
// can pass either the bare repository or a NotifyingAuditWriter
// directly.
type AuditRecorder interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// APIKeyRotationHandlerConfig configures the handler.
type APIKeyRotationHandlerConfig struct {
	Store APIKeyStore
	Audit AuditRecorder
	// NowFn allows tests to drive time deterministically.
	NowFn func() time.Time
}

// NewAPIKeyRotationHandler validates and constructs the handler.
func NewAPIKeyRotationHandler(cfg APIKeyRotationHandlerConfig) (*APIKeyRotationHandler, error) {
	if cfg.Store == nil {
		return nil, errors.New("api_key_rotation: Store required")
	}
	if cfg.NowFn == nil {
		cfg.NowFn = func() time.Time { return time.Now().UTC() }
	}
	return &APIKeyRotationHandler{store: cfg.Store, audit: cfg.Audit, now: cfg.NowFn}, nil
}

// Register mounts the endpoint on rg.
func (h *APIKeyRotationHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/tenants/:tenant_id/rotate-api-key", h.rotate)
}

// APIKeyRotationResponse is the JSON envelope returned once.
type APIKeyRotationResponse struct {
	TenantID   string    `json:"tenant_id"`
	APIKey     string    `json:"api_key"`
	KeyID      string    `json:"key_id"`
	CreatedAt  time.Time `json:"created_at"`
	GraceUntil time.Time `json:"grace_until"`
}

// APIKeyGracePeriod returns the configured grace period from the
// env var, falling back to 24h.
func APIKeyGracePeriod() time.Duration {
	if raw := os.Getenv("CONTEXT_ENGINE_API_KEY_GRACE_PERIOD"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 24 * time.Hour
}

func (h *APIKeyRotationHandler) rotate(c *gin.Context) {
	requesterTenant, _ := c.Get(audit.TenantContextKey)
	rid, _ := requesterTenant.(string)
	if rid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	tenantID := c.Param("tenant_id")
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id required"})
		return
	}
	if tenantID != rid {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-tenant rotation forbidden"})
		return
	}
	ctx := c.Request.Context()
	now := h.now()
	grace := now.Add(APIKeyGracePeriod())
	raw, err := generateAPIKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "generate key"})
		return
	}
	row := &APIKeyRow{
		ID:        ulid.Make().String(),
		TenantID:  tenantID,
		KeyHash:   hashAPIKey(raw),
		Status:    string(APIKeyStatusActive),
		CreatedAt: now,
	}
	if err := h.store.Rotate(ctx, tenantID, grace, row); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rotate key"})
		return
	}
	if h.audit != nil {
		_ = h.audit.Create(ctx, &audit.AuditLog{
			ID:           ulid.Make().String(),
			TenantID:     tenantID,
			Action:       audit.ActionAPIKeyRotated,
			ResourceType: "api_key",
			ResourceID:   row.ID,
			CreatedAt:    now,
		})
	}
	c.JSON(http.StatusOK, APIKeyRotationResponse{
		TenantID:   tenantID,
		APIKey:     raw,
		KeyID:      row.ID,
		CreatedAt:  now,
		GraceUntil: grace,
	})
}

// generateAPIKey produces a 32-byte random key encoded as hex.
func generateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "hf_" + hex.EncodeToString(buf), nil
}

// hashAPIKey returns the SHA-256 hex digest of the supplied key.
// Exported so call sites that need to look up a key by its
// cleartext form can compute the persisted hash.
func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
