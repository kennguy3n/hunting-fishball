-- Rollback for 016_namespace_policies.sql.
DROP INDEX IF EXISTS idx_namespace_policies_tenant;
DROP TABLE IF EXISTS namespace_policies;
