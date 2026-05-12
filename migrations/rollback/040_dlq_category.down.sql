-- Round-14 Task 14 rollback.
DROP INDEX IF EXISTS idx_dlq_messages_tenant_category;
ALTER TABLE dlq_messages DROP COLUMN IF EXISTS category;
