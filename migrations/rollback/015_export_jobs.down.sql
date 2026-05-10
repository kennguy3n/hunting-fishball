-- Rollback for 015_export_jobs.sql.
DROP INDEX IF EXISTS idx_export_jobs_status;
DROP INDEX IF EXISTS idx_export_jobs_tenant;
DROP TABLE IF EXISTS export_jobs;
