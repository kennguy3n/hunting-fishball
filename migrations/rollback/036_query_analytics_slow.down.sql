-- Rollback for 036_query_analytics_slow.sql.
DROP INDEX IF EXISTS idx_query_analytics_slow_tenant_time;
ALTER TABLE query_analytics DROP COLUMN IF EXISTS slow;
