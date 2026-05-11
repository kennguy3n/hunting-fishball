package admin

// notification_gorm.go — Round-9 Task 1.
//
// Postgres-backed NotificationStore. Schema lives in
// migrations/022_notification_preferences.sql (table:
// notification_preferences). The `ListByEvent` path only returns
// rows with `enabled = true`, matching the partial-index condition
// from the migration.

import (
	"context"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// notificationPrefRow is the GORM row representation. The public
// NotificationPreference shape stays unchanged so all existing
// handler/dispatcher code keeps compiling.
type notificationPrefRow struct {
	ID        string    `gorm:"column:id;type:char(26);primaryKey"`
	TenantID  string    `gorm:"column:tenant_id;type:char(26);not null;index:idx_notification_prefs_tenant_event,priority:1"`
	EventType string    `gorm:"column:event_type;type:varchar(64);not null;index:idx_notification_prefs_tenant_event,priority:2"`
	Channel   string    `gorm:"column:channel;type:varchar(16);not null"`
	Target    string    `gorm:"column:target;type:text;not null"`
	Enabled   bool      `gorm:"column:enabled;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

// TableName binds the GORM model to the migration's table.
func (notificationPrefRow) TableName() string { return "notification_preferences" }

// NotificationStoreGORM is the Postgres implementation.
type NotificationStoreGORM struct {
	db   *gorm.DB
	idFn func() string
	now  func() time.Time
}

// NewNotificationStoreGORM validates inputs.
func NewNotificationStoreGORM(db *gorm.DB) (*NotificationStoreGORM, error) {
	if db == nil {
		return nil, errors.New("notification_gorm: nil db")
	}
	return &NotificationStoreGORM{
		db:   db,
		idFn: func() string { return "notif-" + ulid.Make().String() },
		now:  time.Now,
	}, nil
}

// AutoMigrate provisions the table when the SQL migration has not
// yet been applied (used by tests against SQLite).
func (s *NotificationStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&notificationPrefRow{})
}

// List returns every notification preference for the tenant.
func (s *NotificationStoreGORM) List(tenantID string) ([]*NotificationPreference, error) {
	if tenantID == "" {
		return nil, nil
	}
	var rows []notificationPrefRow
	if err := s.db.Where("tenant_id = ?", tenantID).Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*NotificationPreference, 0, len(rows))
	for _, r := range rows {
		p := rowToNotificationPref(r)
		out = append(out, &p)
	}
	return out, nil
}

// ListByEvent returns only enabled preferences for a given event
// type — the dispatcher uses this on every audit fan-out.
func (s *NotificationStoreGORM) ListByEvent(tenantID, eventType string) ([]*NotificationPreference, error) {
	if tenantID == "" || eventType == "" {
		return nil, nil
	}
	var rows []notificationPrefRow
	if err := s.db.
		Where("tenant_id = ? AND event_type = ? AND enabled = ?", tenantID, eventType, true).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*NotificationPreference, 0, len(rows))
	for _, r := range rows {
		p := rowToNotificationPref(r)
		out = append(out, &p)
	}
	return out, nil
}

// Create inserts a new preference row. Mutates `p` in place so the
// caller can return the assigned ID/timestamps to clients.
func (s *NotificationStoreGORM) Create(p *NotificationPreference) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.ID == "" {
		p.ID = s.idFn()
	}
	now := s.now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	row := notificationPrefToRow(*p)
	return s.db.Create(&row).Error
}

// Delete removes a preference by id, scoped to tenant.
func (s *NotificationStoreGORM) Delete(tenantID, id string) error {
	if tenantID == "" || id == "" {
		return errors.New("notification_gorm: missing tenant/id")
	}
	res := s.db.Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&notificationPrefRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("notification: not found")
	}
	return nil
}

func rowToNotificationPref(r notificationPrefRow) NotificationPreference {
	return NotificationPreference{
		ID:        r.ID,
		TenantID:  r.TenantID,
		EventType: r.EventType,
		Channel:   NotificationChannel(r.Channel),
		Target:    r.Target,
		Enabled:   r.Enabled,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

func notificationPrefToRow(p NotificationPreference) notificationPrefRow {
	return notificationPrefRow{
		ID:        p.ID,
		TenantID:  p.TenantID,
		EventType: p.EventType,
		Channel:   string(p.Channel),
		Target:    p.Target,
		Enabled:   p.Enabled,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}
