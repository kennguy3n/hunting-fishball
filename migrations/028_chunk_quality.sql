-- 028_chunk_quality.sql — Round-7 Task 12.
-- Per-chunk quality score recorded by the ChunkScorer pre-write hook.
-- One row per chunk; the GET /v1/admin/chunks/quality-report endpoint
-- aggregates by source for the dashboard.

CREATE TABLE IF NOT EXISTS chunk_quality (
    tenant_id     CHAR(26)         NOT NULL,
    source_id     CHAR(26)         NOT NULL,
    document_id   VARCHAR(128)     NOT NULL,
    chunk_id      VARCHAR(128)     NOT NULL,
    quality_score DOUBLE PRECISION NOT NULL DEFAULT 0,
    length_score  DOUBLE PRECISION NOT NULL DEFAULT 0,
    lang_score    DOUBLE PRECISION NOT NULL DEFAULT 0,
    embed_score   DOUBLE PRECISION NOT NULL DEFAULT 0,
    duplicate     BOOLEAN          NOT NULL DEFAULT FALSE,
    updated_at    TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, chunk_id)
);

CREATE INDEX IF NOT EXISTS idx_chunk_quality_tenant_source
    ON chunk_quality (tenant_id, source_id);
