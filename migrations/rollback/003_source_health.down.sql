-- Rollback for 003_source_health.sql.
--
-- Drops the source_health table. The dashboard handler returns
-- 'unknown' for missing rows, so a deploy that drops this table
-- still serves traffic, just without health badges.
DROP INDEX IF EXISTS idx_source_health_tenant_status;
DROP TABLE IF EXISTS source_health;
