-- 003_source_health.sql
-- Per-source sync health, surfaced by GET /v1/admin/sources/:id/health.
-- Health is derived from three signals:
--
--   last_success_at — timestamp of the most recent successful Stage 4
--                     completion for any document under this source
--   lag             — number of pending Kafka messages for this source
--                     (computed by the consumer; persisted here for the
--                     admin view)
--   error_count     — rolling 24h failure counter
--
-- The pipeline coordinator updates this row on every document
-- completion / failure; the admin GET handler reads it.
--
-- (tenant_id, source_id) is the natural key. Health rows are created
-- lazily on the first observation; the admin handler tolerates a
-- missing row by returning an `unknown` status.

CREATE TABLE IF NOT EXISTS source_health (
    tenant_id        CHAR(26)    NOT NULL,
    source_id        CHAR(26)    NOT NULL,
    last_success_at  TIMESTAMPTZ,
    last_failure_at  TIMESTAMPTZ,
    lag              INTEGER     NOT NULL DEFAULT 0,
    error_count      INTEGER     NOT NULL DEFAULT 0,
    status           VARCHAR(16) NOT NULL DEFAULT 'unknown',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_source_health_tenant_status
    ON source_health (tenant_id, status);
