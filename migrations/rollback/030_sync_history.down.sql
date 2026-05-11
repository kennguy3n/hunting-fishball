-- Rollback for 030_sync_history.sql.
DROP INDEX IF EXISTS idx_sync_history_run;
DROP INDEX IF EXISTS idx_sync_history_source_time;
DROP TABLE IF EXISTS sync_history;
