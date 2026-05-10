-- Rollback for 018_feedback_events.sql.
DROP INDEX IF EXISTS idx_feedback_events_tenant_chunk;
DROP INDEX IF EXISTS idx_feedback_events_tenant_query;
DROP TABLE IF EXISTS feedback_events;
