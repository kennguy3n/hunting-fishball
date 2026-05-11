-- 024_query_analytics.sql — Round-7 Task 4.
-- Per-retrieval query telemetry. One row per successful /v1/retrieve
-- call after the response is returned. The table is partitioned by
-- day so the operational sweeper can drop old partitions cheaply.

CREATE TABLE IF NOT EXISTS query_analytics (
    id              CHAR(26)    NOT NULL,
    tenant_id       CHAR(26)    NOT NULL,
    query_hash      VARCHAR(64) NOT NULL,
    query_text      TEXT        NOT NULL DEFAULT '',
    top_k           INTEGER     NOT NULL DEFAULT 0,
    hit_count       INTEGER     NOT NULL DEFAULT 0,
    cache_hit       BOOLEAN     NOT NULL DEFAULT FALSE,
    latency_ms      INTEGER     NOT NULL DEFAULT 0,
    backend_timings JSONB       NOT NULL DEFAULT '{}',
    experiment_name VARCHAR(64),
    experiment_arm  VARCHAR(16),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
);

CREATE INDEX IF NOT EXISTS idx_query_analytics_tenant_time
    ON query_analytics (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_query_analytics_query_hash
    ON query_analytics (tenant_id, query_hash, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_query_analytics_experiment
    ON query_analytics (tenant_id, experiment_name, experiment_arm, created_at DESC)
    WHERE experiment_name IS NOT NULL;
