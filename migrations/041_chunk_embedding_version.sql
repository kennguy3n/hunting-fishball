-- 041_chunk_embedding_version.sql — Round-18 Task 11.
--
-- Embedding-model versioning per chunk. When a tenant rotates
-- their embedding model via internal/admin/embedding_config.go,
-- the historical chunks in Qdrant carry vectors generated with
-- the previous model — they're "stale" against the new model.
--
-- This table is the index of stale chunks awaiting re-embedding.
-- A background worker walks rows where embedding_model_id differs
-- from the tenant's current model, re-processes the chunk through
-- Stage 3 (Embed), and updates the row.
--
-- The table is intentionally narrow: only the columns the worker
-- needs at scheduling time live here. The chunk body itself stays
-- in object storage / Qdrant.

CREATE TABLE IF NOT EXISTS chunk_embedding_version (
    tenant_id             CHAR(26)     NOT NULL,
    chunk_id              VARCHAR(64)  NOT NULL,
    embedding_model_id    VARCHAR(128) NOT NULL,
    embedding_dimensions  INT          NOT NULL,
    -- Tracks when this chunk was last (re-)embedded. The worker
    -- replays oldest-first to avoid livelocking.
    embedded_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- Set non-NULL when the StaleEmbeddingDetector marks this
    -- chunk for re-embedding. The worker clears this column once
    -- the new vector has been written.
    stale_since           TIMESTAMPTZ,
    -- Latest attempt timestamp + failure reason for ops triage.
    last_attempt_at       TIMESTAMPTZ,
    last_error            TEXT,
    attempts              INT          NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, chunk_id)
);

-- Index for the worker's "find stale chunks for tenant" loop.
CREATE INDEX IF NOT EXISTS idx_chunk_embedding_stale
    ON chunk_embedding_version (tenant_id, stale_since)
    WHERE stale_since IS NOT NULL;

-- Index for the detector's "which chunks use model X?" sweep.
CREATE INDEX IF NOT EXISTS idx_chunk_embedding_model
    ON chunk_embedding_version (tenant_id, embedding_model_id);
