-- Rollback for 009_dlq_messages.sql.
--
-- Drops the dead-letter table. Any in-flight DLQ rows are lost.
-- Operators should drain dlq_messages via the replay handler
-- before applying.
DROP INDEX IF EXISTS idx_dlq_tenant_replayed;
DROP INDEX IF EXISTS idx_dlq_tenant_failed_at;
DROP TABLE IF EXISTS dlq_messages;
