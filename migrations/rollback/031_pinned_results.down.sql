-- Rollback for 031_pinned_results.sql.
DROP INDEX IF EXISTS idx_pinned_results_tenant_pattern;
DROP TABLE IF EXISTS pinned_retrieval_results;
