package admin

// connector_template_gorm.go — Round-9 Task 3.
//
// Postgres-backed ConnectorTemplateStore. Schema lives in
// migrations/023_connector_templates.sql (table:
// connector_templates). DefaultConfig is JSONB, marshalled here via
// the existing admin.JSONMap Scan/Value pair.

import (
	"context"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// connectorTemplateRow is the GORM row representation. The public
// ConnectorTemplate shape stays unchanged so all existing
// handler/audit code keeps compiling.
type connectorTemplateRow struct {
	ID            string    `gorm:"column:id;type:char(26);primaryKey"`
	TenantID      string    `gorm:"column:tenant_id;type:char(26);not null;index:idx_connector_templates_tenant,priority:1"`
	ConnectorType string    `gorm:"column:connector_type;type:varchar(64);not null;index:idx_connector_templates_tenant,priority:2"`
	DefaultConfig JSONMap   `gorm:"column:default_config;type:jsonb;not null;default:'{}'"`
	Description   string    `gorm:"column:description;type:text"`
	CreatedAt     time.Time `gorm:"column:created_at;not null"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null"`
}

// TableName binds the GORM model to the migration's table.
func (connectorTemplateRow) TableName() string { return "connector_templates" }

// ConnectorTemplateStoreGORM is the Postgres implementation.
type ConnectorTemplateStoreGORM struct {
	db   *gorm.DB
	idFn func() string
	now  func() time.Time
}

// NewConnectorTemplateStoreGORM validates inputs.
func NewConnectorTemplateStoreGORM(db *gorm.DB) (*ConnectorTemplateStoreGORM, error) {
	if db == nil {
		return nil, errors.New("connector_template_gorm: nil db")
	}
	return &ConnectorTemplateStoreGORM{
		db:   db,
		idFn: func() string { return "ctmpl-" + ulid.Make().String() },
		now:  time.Now,
	}, nil
}

// AutoMigrate provisions the table when the SQL migration has not
// yet been applied (used by tests against SQLite).
func (s *ConnectorTemplateStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&connectorTemplateRow{})
}

// List returns every template for the tenant.
func (s *ConnectorTemplateStoreGORM) List(tenantID string) ([]*ConnectorTemplate, error) {
	if tenantID == "" {
		return nil, nil
	}
	var rows []connectorTemplateRow
	if err := s.db.Where("tenant_id = ?", tenantID).
		Order("connector_type ASC, created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*ConnectorTemplate, 0, len(rows))
	for _, r := range rows {
		t := rowToConnectorTemplate(r)
		out = append(out, &t)
	}
	return out, nil
}

// Get returns a template by id, scoped to tenant.
func (s *ConnectorTemplateStoreGORM) Get(tenantID, id string) (*ConnectorTemplate, error) {
	if tenantID == "" || id == "" {
		return nil, errors.New("template: not found")
	}
	var r connectorTemplateRow
	if err := s.db.Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("template: not found")
		}
		return nil, err
	}
	t := rowToConnectorTemplate(r)
	return &t, nil
}

// Create inserts a new template row. Mutates `t` in place so the
// caller can return the assigned ID/timestamps to clients.
func (s *ConnectorTemplateStoreGORM) Create(t *ConnectorTemplate) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.ID == "" {
		t.ID = s.idFn()
	}
	now := s.now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	row := connectorTemplateToRow(*t)
	return s.db.Create(&row).Error
}

// Delete removes a template row by id, scoped to tenant.
func (s *ConnectorTemplateStoreGORM) Delete(tenantID, id string) error {
	if tenantID == "" || id == "" {
		return errors.New("template: not found")
	}
	res := s.db.Where("tenant_id = ? AND id = ?", tenantID, id).
		Delete(&connectorTemplateRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("template: not found")
	}
	return nil
}

func rowToConnectorTemplate(r connectorTemplateRow) ConnectorTemplate {
	return ConnectorTemplate{
		ID:            r.ID,
		TenantID:      r.TenantID,
		ConnectorType: r.ConnectorType,
		DefaultConfig: JSONMap(r.DefaultConfig),
		Description:   r.Description,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

func connectorTemplateToRow(t ConnectorTemplate) connectorTemplateRow {
	return connectorTemplateRow{
		ID:            t.ID,
		TenantID:      t.TenantID,
		ConnectorType: t.ConnectorType,
		DefaultConfig: JSONMap(t.DefaultConfig),
		Description:   t.Description,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
}
