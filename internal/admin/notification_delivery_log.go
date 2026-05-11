// notification_delivery_log.go — Round-7 Task 5.
//
// Persists every webhook delivery attempt. The
// NotificationDispatcher invokes Append after each Send (success,
// failure-with-retries, or dead-letter). The handler exposes
// GET /v1/admin/notifications/delivery-log for operators to audit
// recent attempts.
package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// NotificationDeliveryStatus enumerates the lifecycle states of a
// single delivery attempt sequence.
type NotificationDeliveryStatus string

const (
	// NotificationDeliveryStatusDelivered means at least one
	// attempt returned <400.
	NotificationDeliveryStatusDelivered NotificationDeliveryStatus = "delivered"
	// NotificationDeliveryStatusFailed means every attempt
	// returned >=400 or the transport failed.
	NotificationDeliveryStatusFailed NotificationDeliveryStatus = "failed"
	// NotificationDeliveryStatusPending is the transient state
	// recorded before the first attempt completes — used by the
	// retry worker when scheduling future retries.
	NotificationDeliveryStatusPending NotificationDeliveryStatus = "pending"
)

// NotificationDeliveryAttempt is the persisted shape backing the
// migrations/025_notification_delivery_log.sql table.
type NotificationDeliveryAttempt struct {
	ID           string                     `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID     string                     `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	PreferenceID string                     `gorm:"type:char(26);not null;column:preference_id" json:"preference_id"`
	EventType    string                     `gorm:"type:varchar(64);not null;column:event_type" json:"event_type"`
	Channel      NotificationChannel        `gorm:"type:varchar(16);not null;column:channel" json:"channel"`
	Target       string                     `gorm:"type:text;not null;column:target" json:"target"`
	Payload      JSONMap                    `gorm:"type:jsonb;not null;default:'{}';column:payload" json:"payload"`
	Status       NotificationDeliveryStatus `gorm:"type:varchar(16);not null;column:status" json:"status"`
	Attempt      int                        `gorm:"not null;column:attempt" json:"attempt"`
	ResponseCode int                        `gorm:"not null;column:response_code" json:"response_code"`
	ErrorMessage string                     `gorm:"type:text;not null;column:error_message" json:"error_message,omitempty"`
	NextRetryAt  *time.Time                 `gorm:"column:next_retry_at" json:"next_retry_at,omitempty"`
	CreatedAt    time.Time                  `gorm:"not null;default:now();column:created_at" json:"created_at"`
	UpdatedAt    time.Time                  `gorm:"not null;default:now();column:updated_at" json:"updated_at"`
}

// TableName pins the table name.
func (NotificationDeliveryAttempt) TableName() string { return "notification_delivery_log" }

// NotificationDeliveryLog persists delivery attempts.
type NotificationDeliveryLog interface {
	Append(ctx context.Context, attempt *NotificationDeliveryAttempt) error
	List(ctx context.Context, tenantID string, limit int) ([]*NotificationDeliveryAttempt, error)
}

// NotificationDeliveryLogGORM is the Postgres-backed implementation.
type NotificationDeliveryLogGORM struct {
	db *gorm.DB
}

// NewNotificationDeliveryLogGORM constructs the store.
func NewNotificationDeliveryLogGORM(db *gorm.DB) *NotificationDeliveryLogGORM {
	return &NotificationDeliveryLogGORM{db: db}
}

// AutoMigrate ensures the table exists.
func (s *NotificationDeliveryLogGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&NotificationDeliveryAttempt{})
}

// Append inserts a row.
func (s *NotificationDeliveryLogGORM) Append(ctx context.Context, a *NotificationDeliveryAttempt) error {
	if a == nil {
		return errors.New("notification_log: nil attempt")
	}
	if a.TenantID == "" {
		return errors.New("notification_log: missing tenant_id")
	}
	if a.ID == "" {
		a.ID = ulid.Make().String()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	a.UpdatedAt = time.Now().UTC()
	if a.Payload == nil {
		a.Payload = JSONMap{}
	}
	return s.db.WithContext(ctx).Create(a).Error
}

// List returns the most recent attempts for the tenant.
func (s *NotificationDeliveryLogGORM) List(ctx context.Context, tenantID string, limit int) ([]*NotificationDeliveryAttempt, error) {
	if tenantID == "" {
		return nil, errors.New("notification_log: missing tenant_id")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	out := []*NotificationDeliveryAttempt{}
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

// InMemoryNotificationDeliveryLog is the test fake.
type InMemoryNotificationDeliveryLog struct {
	mu      sync.RWMutex
	entries []*NotificationDeliveryAttempt
}

// NewInMemoryNotificationDeliveryLog constructs an empty log.
func NewInMemoryNotificationDeliveryLog() *InMemoryNotificationDeliveryLog {
	return &InMemoryNotificationDeliveryLog{}
}

// Append stores a copy of attempt.
func (s *InMemoryNotificationDeliveryLog) Append(_ context.Context, a *NotificationDeliveryAttempt) error {
	if a == nil {
		return errors.New("notification_log: nil attempt")
	}
	if a.TenantID == "" {
		return errors.New("notification_log: missing tenant_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *a
	if cp.ID == "" {
		cp.ID = ulid.Make().String()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	cp.UpdatedAt = time.Now().UTC()
	s.entries = append(s.entries, &cp)
	return nil
}

// List returns rows for the tenant, newest first.
func (s *InMemoryNotificationDeliveryLog) List(_ context.Context, tenantID string, limit int) ([]*NotificationDeliveryAttempt, error) {
	if tenantID == "" {
		return nil, errors.New("notification_log: missing tenant_id")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*NotificationDeliveryAttempt{}
	for _, e := range s.entries {
		if e.TenantID == tenantID {
			cp := *e
			out = append(out, &cp)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// NotificationDeliveryLogHandler is the admin HTTP surface.
type NotificationDeliveryLogHandler struct {
	log NotificationDeliveryLog
}

// NewNotificationDeliveryLogHandler validates inputs.
func NewNotificationDeliveryLogHandler(log NotificationDeliveryLog) (*NotificationDeliveryLogHandler, error) {
	if log == nil {
		return nil, errors.New("notification_log: nil log")
	}
	return &NotificationDeliveryLogHandler{log: log}, nil
}

// Register mounts GET /v1/admin/notifications/delivery-log.
func (h *NotificationDeliveryLogHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/notifications/delivery-log", h.list)
}

func (h *NotificationDeliveryLogHandler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	limit := 100
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a non-negative integer"})
			return
		}
		if n > 0 {
			limit = n
		}
	}
	rows, err := h.log.List(c.Request.Context(), tenantID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"attempts": rows, "count": len(rows)})
}
