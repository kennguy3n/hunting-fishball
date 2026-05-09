// Package admin implements the Phase 2 B2B admin source-management
// surface. It owns the Postgres-backed `sources` table, the Gin HTTP
// handlers under /v1/admin/sources, the per-source rate limiter, the
// per-source sync-health view, and the forget-on-removal worker.
//
// The package mirrors the conventions used by internal/audit:
//
//   - Tenant isolation enforced at the handler layer; tenant_id comes
//     from the Gin context (populated by auth middleware) and is never
//     read from the request body or query.
//   - GORM-backed model with an explicit migration in
//     migrations/002_sources.sql. The model carries no GORM `index:`
//     tags so AutoMigrate cannot drift from the migration.
//   - Repository methods that surface only typed errors callers can
//     branch on (ErrSourceNotFound, ErrSourceLeased).
//
// The connector-side work (validating credentials, opening a
// connection, listing namespaces) is delegated to the
// internal/connector registry — admin owns the database row and the
// HTTP shape; the connector contract owns the upstream protocol.
package admin

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
)

// SourceStatus is the lifecycle marker on a sources row. The forget
// worker is the only path that flips removing → removed; the admin
// handlers flip the other transitions.
type SourceStatus string

const (
	// SourceStatusActive means the source is ingesting normally.
	SourceStatusActive SourceStatus = "active"
	// SourceStatusPaused means an admin paused the source. The pipeline
	// drops events whose source_id is paused.
	SourceStatusPaused SourceStatus = "paused"
	// SourceStatusRemoving means an admin issued DELETE; the forget
	// worker is purging derived data.
	SourceStatusRemoving SourceStatus = "removing"
	// SourceStatusRemoved is the terminal state — the row is kept for
	// audit purposes but no pipeline work runs against it.
	SourceStatusRemoved SourceStatus = "removed"
)

// JSONMap is a string-keyed JSONB-backed map used for the connector
// config column. Mirrors the JSONMap in internal/audit so both
// packages have identical Scan/Value semantics.
type JSONMap map[string]any

// JSONStringSlice is a JSONB-backed []string used for the scopes
// column. Stored as a JSON array; empty array means "all namespaces".
type JSONStringSlice []string

// Source is the GORM model for the sources table. The schema is
// defined in migrations/002_sources.sql; the model deliberately omits
// GORM `index:` tags so AutoMigrate cannot drift from the migration.
type Source struct {
	// ID is a 26-char ULID. Stored as text for lexicographic time
	// ordering without an extra btree on created_at.
	ID string `gorm:"type:char(26);primaryKey;column:id" json:"id"`

	// TenantID scopes the source to a tenant. Every read query
	// includes `tenant_id = ?`.
	TenantID string `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`

	// ConnectorType names the connector implementation
	// ("google-drive", "slack"). Must match a registered factory.
	ConnectorType string `gorm:"type:varchar(64);not null;column:connector_type" json:"connector_type"`

	// Config holds the connector-specific settings (e.g. SharePoint
	// site URL, Slack workspace ID) plus the credential ciphertext.
	// Credentials are envelope-encrypted by internal/credential
	// before being persisted.
	Config JSONMap `gorm:"type:jsonb;not null;default:'{}';column:config" json:"config"`

	// Scopes is a JSON array of namespace filters. Empty array ==
	// "all namespaces visible to the credential".
	Scopes JSONStringSlice `gorm:"type:jsonb;not null;default:'[]';column:scopes" json:"scopes"`

	// Status is the lifecycle marker. See SourceStatus.
	Status SourceStatus `gorm:"type:varchar(16);not null;default:'active';column:status" json:"status"`

	CreatedAt time.Time `gorm:"not null;default:now();column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null;default:now();column:updated_at" json:"updated_at"`
}

// TableName overrides the default GORM pluralization.
func (Source) TableName() string { return "sources" }

// NewSource constructs a Source with a fresh ULID and sane defaults.
// Callers can override Status by setting the field after construction.
func NewSource(tenantID, connectorType string, config JSONMap, scopes []string) *Source {
	now := time.Now().UTC()
	cp := JSONMap{}
	for k, v := range config {
		cp[k] = v
	}
	sc := JSONStringSlice{}
	if len(scopes) > 0 {
		sc = append(sc, scopes...)
	}

	return &Source{
		ID:            ulid.Make().String(),
		TenantID:      tenantID,
		ConnectorType: connectorType,
		Config:        cp,
		Scopes:        sc,
		Status:        SourceStatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// ErrInvalidSource is returned by Validate when a constructed Source
// is missing required fields.
var ErrInvalidSource = errors.New("admin: invalid source")

// Validate checks the invariants the table column constraints would
// otherwise reject at insert time.
func (s *Source) Validate() error {
	if s.ID == "" {
		return errors.New("admin: missing ID")
	}
	if s.TenantID == "" {
		return errors.New("admin: missing tenant_id")
	}
	if s.ConnectorType == "" {
		return errors.New("admin: missing connector_type")
	}
	switch s.Status {
	case SourceStatusActive, SourceStatusPaused, SourceStatusRemoving, SourceStatusRemoved:
	default:
		return errors.New("admin: invalid status")
	}

	return nil
}

// --- JSONMap Scan/Value (mirrors internal/audit/jsonmap.go) ---

// MarshalJSON normalises nil to {}.
func (m JSONMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(map[string]any(m))
}

// UnmarshalJSON parses the JSON object into the receiver.
func (m *JSONMap) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*m = JSONMap{}
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*m = out
	return nil
}

// Value implements driver.Valuer.
func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(map[string]any(m))
}

// Scan implements sql.Scanner.
func (m *JSONMap) Scan(src any) error {
	if src == nil {
		*m = JSONMap{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("admin: JSONMap.Scan: unsupported source type")
	}
	if len(b) == 0 {
		*m = JSONMap{}
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*m = out
	return nil
}

// --- JSONStringSlice Scan/Value ---

// MarshalJSON normalises nil to [].
func (s JSONStringSlice) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(s))
}

// UnmarshalJSON parses the JSON array.
func (s *JSONStringSlice) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*s = JSONStringSlice{}
		return nil
	}
	out := []string{}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*s = out
	return nil
}

// Value implements driver.Valuer.
func (s JSONStringSlice) Value() (driver.Value, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(s))
}

// Scan implements sql.Scanner.
func (s *JSONStringSlice) Scan(src any) error {
	if src == nil {
		*s = JSONStringSlice{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("admin: JSONStringSlice.Scan: unsupported source type")
	}
	if len(b) == 0 {
		*s = JSONStringSlice{}
		return nil
	}
	out := []string{}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*s = out
	return nil
}
