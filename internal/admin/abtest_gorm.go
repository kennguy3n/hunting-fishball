package admin

// abtest_gorm.go — Round-9 Task 2.
//
// Postgres-backed ABTestStore. Schema lives in
// migrations/021_ab_tests.sql (table: retrieval_ab_tests, natural PK
// on (tenant_id, experiment_name)). Both control and variant
// configs are JSONB columns, marshalled here via the existing
// admin.JSONMap Scan/Value pair.

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// abtestRow is the GORM row representation. ControlConfig and
// VariantConfig are stored as JSONB so the schema is open to future
// config knobs without migration churn.
type abtestRow struct {
	TenantID            string     `gorm:"column:tenant_id;type:char(26);primaryKey"`
	ExperimentName      string     `gorm:"column:experiment_name;type:varchar(64);primaryKey"`
	Status              string     `gorm:"column:status;type:varchar(16);not null;default:draft"`
	TrafficSplitPercent int        `gorm:"column:traffic_split_percent;not null;default:0"`
	ControlConfig       JSONMap    `gorm:"column:control_config;type:jsonb;not null;default:'{}'"`
	VariantConfig       JSONMap    `gorm:"column:variant_config;type:jsonb;not null;default:'{}'"`
	StartAt             *time.Time `gorm:"column:start_at"`
	EndAt               *time.Time `gorm:"column:end_at"`
	CreatedAt           time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt           time.Time  `gorm:"column:updated_at;not null"`
}

// TableName binds the GORM model to the migration's table.
func (abtestRow) TableName() string { return "retrieval_ab_tests" }

// ABTestStoreGORM is the Postgres implementation.
type ABTestStoreGORM struct {
	db  *gorm.DB
	now func() time.Time
}

// NewABTestStoreGORM validates inputs.
func NewABTestStoreGORM(db *gorm.DB) (*ABTestStoreGORM, error) {
	if db == nil {
		return nil, errors.New("abtest_gorm: nil db")
	}
	return &ABTestStoreGORM{db: db, now: time.Now}, nil
}

// AutoMigrate provisions the table when the SQL migration has not
// yet been applied (used by tests against SQLite).
func (s *ABTestStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&abtestRow{})
}

// List returns every experiment for the tenant.
func (s *ABTestStoreGORM) List(tenantID string) ([]*ABTestConfig, error) {
	if tenantID == "" {
		return nil, nil
	}
	var rows []abtestRow
	if err := s.db.Where("tenant_id = ?", tenantID).
		Order("experiment_name ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*ABTestConfig, 0, len(rows))
	for _, r := range rows {
		cfg := rowToABTest(r)
		out = append(out, &cfg)
	}
	return out, nil
}

// Get returns the experiment by name. Returns an error when no row
// matches — mirroring the in-memory implementation.
func (s *ABTestStoreGORM) Get(tenantID, name string) (*ABTestConfig, error) {
	if tenantID == "" || name == "" {
		return nil, errors.New("abtest_gorm: missing tenant/name")
	}
	var r abtestRow
	if err := s.db.Where("tenant_id = ? AND experiment_name = ?", tenantID, name).
		First(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("ab test: not found")
		}
		return nil, err
	}
	cfg := rowToABTest(r)
	return &cfg, nil
}

// Upsert inserts or updates the experiment in place.
func (s *ABTestStoreGORM) Upsert(cfg *ABTestConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	now := s.now().UTC()
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = now
	}
	cfg.UpdatedAt = now
	row := abtestToRow(*cfg)
	return s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "experiment_name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"status", "traffic_split_percent",
			"control_config", "variant_config",
			"start_at", "end_at", "updated_at",
		}),
	}).Create(&row).Error
}

// Delete removes the experiment row by name, scoped to tenant.
func (s *ABTestStoreGORM) Delete(tenantID, name string) error {
	if tenantID == "" || name == "" {
		return errors.New("abtest_gorm: missing tenant/name")
	}
	return s.db.Where("tenant_id = ? AND experiment_name = ?", tenantID, name).
		Delete(&abtestRow{}).Error
}

func rowToABTest(r abtestRow) ABTestConfig {
	return ABTestConfig{
		TenantID:            r.TenantID,
		ExperimentName:      r.ExperimentName,
		Status:              ABTestStatus(r.Status),
		TrafficSplitPercent: r.TrafficSplitPercent,
		ControlConfig:       map[string]any(r.ControlConfig),
		VariantConfig:       map[string]any(r.VariantConfig),
		StartAt:             r.StartAt,
		EndAt:               r.EndAt,
		CreatedAt:           r.CreatedAt,
		UpdatedAt:           r.UpdatedAt,
	}
}

func abtestToRow(c ABTestConfig) abtestRow {
	return abtestRow{
		TenantID:            c.TenantID,
		ExperimentName:      c.ExperimentName,
		Status:              string(c.Status),
		TrafficSplitPercent: c.TrafficSplitPercent,
		ControlConfig:       JSONMap(c.ControlConfig),
		VariantConfig:       JSONMap(c.VariantConfig),
		StartAt:             c.StartAt,
		EndAt:               c.EndAt,
		CreatedAt:           c.CreatedAt,
		UpdatedAt:           c.UpdatedAt,
	}
}
