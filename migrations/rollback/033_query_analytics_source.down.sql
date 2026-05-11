-- Rollback for 033_query_analytics_source.sql.
DROP INDEX IF EXISTS idx_query_analytics_tenant_source_time;
ALTER TABLE query_analytics DROP COLUMN IF EXISTS source;
