-- Rollback for 010_retention_policy.sql.
--
-- Drops the retention policy table. The retention worker
-- short-circuits when no rows are configured, so a rollback
-- pauses TTL-based cleanup but does not affect serving.
DROP INDEX IF EXISTS idx_retention_tenant;
DROP TABLE IF EXISTS retention_policies;
