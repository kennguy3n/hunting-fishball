-- 009_dlq_messages.sql
-- Phase 8 / Task 5: persistent DLQ store. The pipeline DLQ producer
-- (internal/pipeline/consumer.go::publishDLQ) emits an envelope onto
-- the dead-letter Kafka topic; the new dlq_consumer subscribes to
-- that topic and lands each envelope here so operators can inspect
-- and replay failed events from the admin portal.
--
-- Indexes are tenant-scoped because every read goes through the
-- admin handler with tenant_id pulled from the gin context.
--
-- Replay semantics:
--   replayed_at IS NULL  → eligible for replay
--   replayed_at IS NOT NULL  → already replayed; replay endpoint
--                              refuses duplicate replays unless the
--                              admin explicitly forces it.

CREATE TABLE IF NOT EXISTS dlq_messages (
    id              CHAR(26)    NOT NULL PRIMARY KEY,
    tenant_id       CHAR(26)    NOT NULL,
    source_id       VARCHAR(128),
    document_id     VARCHAR(256),
    original_topic  VARCHAR(128) NOT NULL,
    partition_key   TEXT,
    payload         BYTEA       NOT NULL,
    error_text      TEXT        NOT NULL,
    failed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempt_count   INT         NOT NULL DEFAULT 0,
    replayed_at     TIMESTAMPTZ,
    replay_error    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_dlq_tenant_failed_at
    ON dlq_messages (tenant_id, failed_at DESC);

CREATE INDEX IF NOT EXISTS idx_dlq_tenant_replayed
    ON dlq_messages (tenant_id, replayed_at);
