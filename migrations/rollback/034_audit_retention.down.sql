-- Rollback for 034_audit_retention.sql.
DROP INDEX IF EXISTS idx_audit_logs_created_at;
