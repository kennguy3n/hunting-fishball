-- Rollback for 024_query_analytics.sql.
DROP INDEX IF EXISTS idx_query_analytics_experiment;
DROP INDEX IF EXISTS idx_query_analytics_query_hash;
DROP INDEX IF EXISTS idx_query_analytics_tenant_time;
DROP TABLE IF EXISTS query_analytics;
