-- 019_source_embedding_config.sql — Round-6 Task 3.
-- Per-source embedding model configuration. Operators set a
-- non-default `model_name` / `dimensions` on a per-(tenant, source)
-- basis when a tenant requires a higher-fidelity (or smaller, for
-- on-device parity) embedding family than the global default.
--
-- The pipeline embed stage joins this table on (tenant_id, source_id)
-- to pick the model for the current document; missing rows fall back
-- to the global default the embedding service was deployed with.

CREATE TABLE IF NOT EXISTS source_embedding_config (
    tenant_id    CHAR(26)     NOT NULL,
    source_id    CHAR(26)     NOT NULL,
    model_name   VARCHAR(128) NOT NULL,
    dimensions   INTEGER      NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_source_embedding_config_tenant
    ON source_embedding_config (tenant_id);
