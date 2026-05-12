-- Rollback for 038_slow_queries.sql.
DROP INDEX IF EXISTS idx_slow_queries_tenant_latency;
DROP INDEX IF EXISTS idx_slow_queries_tenant_created;
DROP TABLE IF EXISTS slow_queries;
