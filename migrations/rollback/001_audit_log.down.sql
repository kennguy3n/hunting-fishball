-- Rollback for 001_audit_log.sql.
--
-- WARNING: dropping audit_logs is irreversible. The forget worker
-- and tenant deletion flow rely on this table; dropping it
-- without a downstream archive deletes the tenant compliance
-- record. Operators should EXPORT the table before applying.
DROP INDEX IF EXISTS idx_audit_unpublished;
DROP INDEX IF EXISTS idx_audit_tenant_action;
DROP INDEX IF EXISTS idx_audit_tenant_actor;
DROP INDEX IF EXISTS idx_audit_tenant_created;
DROP TABLE IF EXISTS audit_logs;
