-- Rollback for 025_notification_delivery_log.sql.
DROP INDEX IF EXISTS idx_notif_delivery_status;
DROP INDEX IF EXISTS idx_notif_delivery_tenant_time;
DROP TABLE IF EXISTS notification_delivery_log;
