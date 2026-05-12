-- 038_slow_queries.sql — Round-14 Task 3.
--
-- Persistent slow-query log. Round-13 Task 8 added a `slow`
-- column on the existing query_analytics table so the slow-query
-- admin endpoint could filter rows in place. That column is
-- still authoritative for the recent-rows admin view, but
-- query_analytics is also the rollup table that powers the
-- top-N / per-tenant analytics surface — keeping every slow
-- retrieval alongside the rollup forces the analytics path to
-- carry per-row latency detail it does not need.
--
-- The slow_queries table separates the long-tail "what was
-- pathologically slow last hour" detail from the rollup. Rows
-- are tenant-scoped and time-ordered (`created_at DESC` is the
-- only query order). The per-backend timings live in a JSONB
-- column so the schema stays flexible as new backends land.
--
-- Operators are expected to retain slow_queries for a bounded
-- window (e.g. 7 days) — there is no foreign-key relationship
-- to query_analytics, so a separate retention sweep can
-- truncate it independently.

CREATE TABLE IF NOT EXISTS slow_queries (
    id              CHAR(26)     NOT NULL PRIMARY KEY,
    tenant_id       CHAR(26)     NOT NULL,
    query_hash      VARCHAR(64)  NOT NULL,
    query_text      TEXT         NOT NULL,
    latency_ms      INTEGER      NOT NULL,
    top_k           INTEGER      NOT NULL DEFAULT 0,
    hit_count       INTEGER      NOT NULL DEFAULT 0,
    backend_timings JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_slow_queries_tenant_created
    ON slow_queries (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_slow_queries_tenant_latency
    ON slow_queries (tenant_id, latency_ms DESC);
