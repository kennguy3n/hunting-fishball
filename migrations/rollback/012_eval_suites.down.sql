-- Rollback for 012_eval_suites.sql.
DROP INDEX IF EXISTS idx_eval_suites_tenant;
DROP INDEX IF EXISTS idx_eval_suites_tenant_name;
DROP TABLE IF EXISTS eval_suites;
