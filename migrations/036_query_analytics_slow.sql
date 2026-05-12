-- 036_query_analytics_slow.sql — Round-13 Task 8.
--
-- Adds a `slow` boolean column to query_analytics so operators can
-- list every retrieval that crossed the per-deployment slow-query
-- threshold (CONTEXT_ENGINE_SLOW_QUERY_THRESHOLD_MS, default
-- 1000ms). Defaults to false so backfilled rows keep their
-- semantics: only rows recorded after the new threshold logic
-- ships will be flagged.
--
-- Allowed values are constrained by the column type (boolean);
-- the application layer is the source of truth for the
-- threshold itself.

ALTER TABLE query_analytics
    ADD COLUMN IF NOT EXISTS slow BOOLEAN NOT NULL DEFAULT FALSE;

-- Partial index on (tenant_id, created_at DESC) WHERE slow=true.
-- The slow-query admin endpoint always filters by slow=true and
-- is by far the most expensive listing, so a partial index keeps
-- the index footprint small. A small minority of rows are slow
-- under healthy conditions, so the partial index stays cheap.
CREATE INDEX IF NOT EXISTS idx_query_analytics_slow_tenant_time
    ON query_analytics (tenant_id, created_at DESC)
    WHERE slow = TRUE;
