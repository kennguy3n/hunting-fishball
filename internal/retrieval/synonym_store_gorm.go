package retrieval

// synonym_store_gorm.go — Round-9 Task 4.
//
// Postgres-backed SynonymStore. Schema lives in
// migrations/032_synonyms.sql (table: retrieval_synonyms, PK on
// (tenant_id, term)). Synonym lists are stored as a JSONB array
// per term so the GET path can stream rows back without joining
// against another table.
//
// Set() replaces the tenant's entire map atomically: it wipes the
// existing rows for the tenant and inserts the new ones inside a
// single transaction. This matches the in-memory store's semantics
// where a fresh write fully supersedes the previous map.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// synonymList is a JSONB-backed []string. Mirrors the
// JSONStringSlice in internal/admin so the retrieval package
// doesn't have to import admin for Scan/Value semantics.
type synonymList []string

// Value implements driver.Valuer.
func (s synonymList) Value() (driver.Value, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(s))
}

// Scan implements sql.Scanner.
func (s *synonymList) Scan(src any) error {
	if src == nil {
		*s = synonymList{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("retrieval: synonymList.Scan: unsupported source type")
	}
	if len(b) == 0 {
		*s = synonymList{}
		return nil
	}
	out := []string{}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*s = out
	return nil
}

// synonymRow is the GORM row representation.
type synonymRow struct {
	TenantID  string      `gorm:"column:tenant_id;type:char(26);primaryKey"`
	Term      string      `gorm:"column:term;type:varchar(64);primaryKey"`
	Synonyms  synonymList `gorm:"column:synonyms;type:jsonb;not null;default:'[]'"`
	UpdatedAt time.Time   `gorm:"column:updated_at;not null"`
}

// TableName binds the GORM model to the migration's table.
func (synonymRow) TableName() string { return "retrieval_synonyms" }

// SynonymStoreGORM is the Postgres-backed implementation.
type SynonymStoreGORM struct {
	db  *gorm.DB
	now func() time.Time
}

// NewSynonymStoreGORM validates inputs.
func NewSynonymStoreGORM(db *gorm.DB) (*SynonymStoreGORM, error) {
	if db == nil {
		return nil, errors.New("synonym_store_gorm: nil db")
	}
	return &SynonymStoreGORM{db: db, now: time.Now}, nil
}

// AutoMigrate provisions the table when the SQL migration has not
// yet been applied (used by tests against SQLite).
func (s *SynonymStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&synonymRow{})
}

// Get returns the tenant's synonym map. Empty map (or nil) when no
// synonyms are configured for the tenant.
func (s *SynonymStoreGORM) Get(ctx context.Context, tenantID string) (map[string][]string, error) {
	if tenantID == "" {
		return nil, nil
	}
	var rows []synonymRow
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		cp := append([]string(nil), r.Synonyms...)
		out[r.Term] = cp
	}
	return out, nil
}

// Set replaces the tenant's synonym map atomically. A nil or empty
// map clears all rows for the tenant.
func (s *SynonymStoreGORM) Set(ctx context.Context, tenantID string, synonyms map[string][]string) error {
	if tenantID == "" {
		return errors.New("synonym_store_gorm: missing tenant_id")
	}
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ?", tenantID).Delete(&synonymRow{}).Error; err != nil {
			return err
		}
		if len(synonyms) == 0 {
			return nil
		}
		rows := make([]synonymRow, 0, len(synonyms))
		for term, alts := range synonyms {
			key := strings.ToLower(strings.TrimSpace(term))
			if key == "" {
				continue
			}
			cp := append([]string(nil), alts...)
			rows = append(rows, synonymRow{
				TenantID:  tenantID,
				Term:      key,
				Synonyms:  cp,
				UpdatedAt: now,
			})
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
}
