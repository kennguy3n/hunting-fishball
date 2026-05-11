-- Rollback for 023_connector_templates.sql.
DROP INDEX IF EXISTS idx_connector_templates_tenant;
DROP TABLE IF EXISTS connector_templates;
