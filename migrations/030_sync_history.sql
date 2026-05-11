-- 030_sync_history.sql — Round-7 Task 16.
-- Historical record of connector sync runs. One row per
-- (tenant_id, source_id, sync_run_id); the pipeline consumer
-- records sync boundaries and the admin endpoint pages over them.

CREATE TABLE IF NOT EXISTS sync_history (
    id             CHAR(26)     NOT NULL,
    tenant_id      CHAR(26)     NOT NULL,
    source_id      CHAR(26)     NOT NULL,
    sync_run_id    VARCHAR(64)  NOT NULL,
    status         VARCHAR(16)  NOT NULL DEFAULT 'running',
    docs_processed INTEGER      NOT NULL DEFAULT 0,
    docs_failed    INTEGER      NOT NULL DEFAULT 0,
    started_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    ended_at       TIMESTAMPTZ,
    error_message  TEXT         NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_sync_history_source_time
    ON sync_history (tenant_id, source_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sync_history_run
    ON sync_history (tenant_id, source_id, sync_run_id);
