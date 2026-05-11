-- 029_cache_config.sql — Round-7 Task 15.
-- Per-tenant override for the Redis semantic-cache TTL. The
-- retrieval handler picks the per-tenant value (falling back to the
-- global default) when populating cache entries.

CREATE TABLE IF NOT EXISTS tenant_cache_config (
    tenant_id  CHAR(26)    NOT NULL,
    ttl_ms     INTEGER     NOT NULL DEFAULT 0,
    notes      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id)
);
