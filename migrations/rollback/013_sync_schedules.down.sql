-- Rollback for 013_sync_schedules.sql.
DROP INDEX IF EXISTS idx_sync_schedules_tenant;
DROP INDEX IF EXISTS idx_sync_schedules_due;
DROP INDEX IF EXISTS uq_sync_schedules_tenant_source;
DROP TABLE IF EXISTS sync_schedules;
