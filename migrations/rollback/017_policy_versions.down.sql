-- Rollback for 017_policy_versions.sql.
DROP INDEX IF EXISTS idx_policy_versions_tenant_channel;
DROP INDEX IF EXISTS idx_policy_versions_tenant;
DROP TABLE IF EXISTS policy_versions;
