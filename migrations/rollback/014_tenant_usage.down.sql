-- Rollback for 014_tenant_usage.sql.
DROP INDEX IF EXISTS idx_tenant_usage_day;
DROP TABLE IF EXISTS tenant_usage;
