-- 033_query_analytics_source.sql — Round-11 Task 9.
--
-- Adds a `source` column to query_analytics so operators can tell
-- organic /v1/retrieve traffic apart from cache-warm jobs and
-- batched calls. Defaults to "user" so the existing rows keep
-- their semantics (every pre-Round-11 row was a user-initiated
-- retrieve).
--
-- Allowed values are enforced by the application layer (the
-- QueryAnalyticsEvent.Source string is one of "user" |
-- "cache_warm" | "batch"); the column is varchar to keep the
-- migration cheap (no CHECK constraint to rewrite if we add a
-- new source later).

ALTER TABLE query_analytics
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'user';

-- Index on (tenant_id, source, created_at) speeds the new
-- analytics dashboard's "user-only" rollup. We deliberately do
-- NOT replace the existing tenant/time index because a query
-- without a source filter still needs the original.
CREATE INDEX IF NOT EXISTS idx_query_analytics_tenant_source_time
    ON query_analytics (tenant_id, source, created_at DESC);
