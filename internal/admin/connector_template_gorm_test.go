package admin_test

// connector_template_gorm_test.go — Round-9 Task 3.

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// connectorTemplateSQLiteDDL mirrors
// migrations/023_connector_templates.sql with SQLite-compatible
// types. TEXT replaces JSONB; JSONMap.Scan/Value still operate over
// the TEXT JSON string.
const connectorTemplateSQLiteDDL = `CREATE TABLE IF NOT EXISTS connector_templates (
id              TEXT NOT NULL PRIMARY KEY,
tenant_id       TEXT NOT NULL,
connector_type  TEXT NOT NULL,
default_config  TEXT NOT NULL DEFAULT '{}',
description     TEXT,
created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func newConnectorTemplateStoreGORMForTest(t *testing.T) *admin.ConnectorTemplateStoreGORM {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.Exec(connectorTemplateSQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := admin.NewConnectorTemplateStoreGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

func TestConnectorTemplateStoreGORM_CreateAndGet(t *testing.T) {
	t.Parallel()
	s := newConnectorTemplateStoreGORMForTest(t)
	tmpl := &admin.ConnectorTemplate{
		TenantID:      "t-1",
		ConnectorType: "slack",
		DefaultConfig: admin.JSONMap{"workspace_id": "W123", "scopes": []any{"channels:read"}},
		Description:   "company slack defaults",
	}
	if err := s.Create(tmpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	if tmpl.ID == "" {
		t.Fatalf("expected generated id")
	}
	got, err := s.Get("t-1", tmpl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConnectorType != "slack" {
		t.Fatalf("connector type: %s", got.ConnectorType)
	}
	if got.DefaultConfig["workspace_id"] != "W123" {
		t.Fatalf("default_config: %+v", got.DefaultConfig)
	}
}

func TestConnectorTemplateStoreGORM_List(t *testing.T) {
	t.Parallel()
	s := newConnectorTemplateStoreGORMForTest(t)
	for _, ct := range []string{"slack", "drive", "notion"} {
		_ = s.Create(&admin.ConnectorTemplate{
			TenantID: "t-1", ConnectorType: ct, DefaultConfig: admin.JSONMap{},
		})
	}
	rows, err := s.List("t-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(rows))
	}
}

func TestConnectorTemplateStoreGORM_TenantIsolation(t *testing.T) {
	t.Parallel()
	s := newConnectorTemplateStoreGORMForTest(t)
	_ = s.Create(&admin.ConnectorTemplate{TenantID: "ta", ConnectorType: "slack", DefaultConfig: admin.JSONMap{}})
	_ = s.Create(&admin.ConnectorTemplate{TenantID: "tb", ConnectorType: "slack", DefaultConfig: admin.JSONMap{}})
	rows, _ := s.List("ta")
	if len(rows) != 1 || rows[0].TenantID != "ta" {
		t.Fatalf("cross-tenant leak: %+v", rows)
	}
}

func TestConnectorTemplateStoreGORM_Delete(t *testing.T) {
	t.Parallel()
	s := newConnectorTemplateStoreGORMForTest(t)
	tmpl := &admin.ConnectorTemplate{TenantID: "t-1", ConnectorType: "slack", DefaultConfig: admin.JSONMap{}}
	if err := s.Create(tmpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete("t-other", tmpl.ID); err == nil {
		t.Fatalf("expected delete from wrong tenant to fail")
	}
	if err := s.Delete("t-1", tmpl.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get("t-1", tmpl.ID); err == nil {
		t.Fatalf("expected not-found after delete")
	}
}

func TestConnectorTemplateStoreGORM_ValidateOnCreate(t *testing.T) {
	t.Parallel()
	s := newConnectorTemplateStoreGORMForTest(t)
	bad := &admin.ConnectorTemplate{TenantID: "", ConnectorType: "slack", DefaultConfig: admin.JSONMap{}}
	if err := s.Create(bad); err == nil {
		t.Fatalf("expected validation error")
	}
}

func TestConnectorTemplateStoreGORM_GetMissing(t *testing.T) {
	t.Parallel()
	s := newConnectorTemplateStoreGORMForTest(t)
	if _, err := s.Get("t-1", "no-such-id"); err == nil {
		t.Fatalf("expected not-found")
	}
}
