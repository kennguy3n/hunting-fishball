-- Rollback for 037_api_keys.sql.
DROP INDEX IF EXISTS idx_api_keys_tenant_status;
DROP TABLE IF EXISTS api_keys;
