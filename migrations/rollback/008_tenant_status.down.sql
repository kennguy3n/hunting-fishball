-- Rollback for 008_tenant_status.sql.
DROP INDEX IF EXISTS idx_tenants_status;
DROP TABLE IF EXISTS tenants;
