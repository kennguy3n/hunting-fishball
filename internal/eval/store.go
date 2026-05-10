package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// SuiteRow is the GORM model for the eval_suites table defined in
// migrations/007_eval_suites.sql. Cases are stored as JSONB so the
// admin portal can edit them as a unit; the table key is the
// (tenant_id, name) tuple plus a generated ULID for primary identity.
type SuiteRow struct {
	ID        string    `gorm:"type:varchar(26);primaryKey;column:id"`
	TenantID  string    `gorm:"type:varchar(26);not null;column:tenant_id;uniqueIndex:idx_eval_suite_tenant_name,priority:1"`
	Name      string    `gorm:"type:varchar(128);not null;column:name;uniqueIndex:idx_eval_suite_tenant_name,priority:2"`
	Corpus    []byte    `gorm:"type:jsonb;not null;column:corpus"`
	DefaultK  int       `gorm:"not null;default:0;column:default_k"`
	CreatedAt time.Time `gorm:"not null;default:now();column:created_at"`
	UpdatedAt time.Time `gorm:"not null;default:now();column:updated_at"`
}

// TableName overrides the default GORM pluralisation.
func (SuiteRow) TableName() string { return "eval_suites" }

// SuiteRepository persists and loads EvalSuites from Postgres.
// In-memory stores back the unit tests (see store_test.go).
type SuiteRepository interface {
	Save(ctx context.Context, suite EvalSuite) error
	Get(ctx context.Context, tenantID, name string) (EvalSuite, error)
	List(ctx context.Context, tenantID string) ([]EvalSuite, error)
}

// ErrSuiteNotFound is returned by Get when no row matches.
var ErrSuiteNotFound = errors.New("eval: suite not found")

// SuiteRepositoryGORM is the production SuiteRepository.
type SuiteRepositoryGORM struct {
	db *gorm.DB
}

// NewSuiteRepository returns a GORM-backed repository.
func NewSuiteRepository(db *gorm.DB) *SuiteRepositoryGORM {
	return &SuiteRepositoryGORM{db: db}
}

// AutoMigrate creates the eval_suites table when no migration runner
// is installed (e.g. during in-process tests).
func (r *SuiteRepositoryGORM) AutoMigrate(ctx context.Context) error {
	return r.db.WithContext(ctx).AutoMigrate(&SuiteRow{})
}

// Save upserts a suite. Cases are JSON-encoded into the corpus
// column. The (tenant_id, name) unique index promotes name reuse
// to "edit-in-place" semantics rather than dup rows.
func (r *SuiteRepositoryGORM) Save(ctx context.Context, suite EvalSuite) error {
	if err := suite.Validate(); err != nil {
		return err
	}
	corpus, err := json.Marshal(suite.Cases)
	if err != nil {
		return fmt.Errorf("eval: marshal corpus: %w", err)
	}
	now := time.Now().UTC()
	row := SuiteRow{
		TenantID:  suite.TenantID,
		Name:      suite.Name,
		Corpus:    corpus,
		DefaultK:  suite.DefaultK,
		UpdatedAt: now,
	}
	tx := r.db.WithContext(ctx)

	var existing SuiteRow
	err = tx.Where("tenant_id = ? AND name = ?", suite.TenantID, suite.Name).
		Limit(1).Find(&existing).Error
	if err != nil {
		return fmt.Errorf("eval: lookup suite: %w", err)
	}
	if existing.ID == "" {
		row.ID = ulid.Make().String()
		row.CreatedAt = now
		return tx.Create(&row).Error
	}
	row.ID = existing.ID
	row.CreatedAt = existing.CreatedAt
	return tx.Save(&row).Error
}

// Get loads a suite by (tenantID, name).
func (r *SuiteRepositoryGORM) Get(ctx context.Context, tenantID, name string) (EvalSuite, error) {
	var row SuiteRow
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", tenantID, name).
		Limit(1).Find(&row).Error
	if err != nil {
		return EvalSuite{}, err
	}
	if row.ID == "" {
		return EvalSuite{}, ErrSuiteNotFound
	}
	return rowToSuite(row)
}

// List enumerates suites for a tenant in name order.
func (r *SuiteRepositoryGORM) List(ctx context.Context, tenantID string) ([]EvalSuite, error) {
	var rows []SuiteRow
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("name ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]EvalSuite, 0, len(rows))
	for _, row := range rows {
		s, err := rowToSuite(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func rowToSuite(row SuiteRow) (EvalSuite, error) {
	var cases []EvalCase
	if len(row.Corpus) > 0 {
		if err := json.Unmarshal(row.Corpus, &cases); err != nil {
			return EvalSuite{}, fmt.Errorf("eval: unmarshal corpus: %w", err)
		}
	}
	return EvalSuite{
		Name:     row.Name,
		TenantID: row.TenantID,
		Cases:    cases,
		DefaultK: row.DefaultK,
	}, nil
}
