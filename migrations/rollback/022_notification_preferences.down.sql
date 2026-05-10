-- Rollback for 022_notification_preferences.sql.
DROP INDEX IF EXISTS idx_notification_prefs_tenant_event;
DROP TABLE IF EXISTS notification_preferences;
