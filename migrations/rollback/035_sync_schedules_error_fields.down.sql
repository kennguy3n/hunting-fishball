-- Rollback for 035_sync_schedules_error_fields.sql.
--
-- Drops the two error-tracking columns from sync_schedules. After
-- rollback, scheduler.Tick must not be writing to these columns
-- (the matching Go change splits the success-path UPDATE so the
-- error-clearing branch is best-effort and tolerates absent
-- columns) — otherwise the success-path UPDATE will fail again.

ALTER TABLE sync_schedules DROP COLUMN IF EXISTS last_error_at;
ALTER TABLE sync_schedules DROP COLUMN IF EXISTS last_error;
