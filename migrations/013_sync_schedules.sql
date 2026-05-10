-- 013_sync_schedules.sql
--
-- Phase 8 / Round-4 Task 5: source-sync cron scheduler.
--
-- Each row is a per-(tenant, source) cron schedule. The scheduler
-- goroutine in cmd/ingest polls for rows where enabled=true and
-- next_run_at <= now() every minute (configurable). The (tenant_id,
-- source_id) tuple is unique — one schedule per source. Rotating
-- the cron expression issues an UPDATE on the existing row.
--
-- Index notes:
--   * idx_sync_schedules_due is the hot path used by the scheduler
--     loop. It serves the predicate `enabled = TRUE AND next_run_at
--     <= now()`. Postgres's planner prefers a btree on (next_run_at,
--     enabled) over a partial index here because the loop is small
--     and predicate selectivity tilts on next_run_at first.
--   * idx_sync_schedules_tenant gives the admin listing endpoint
--     fast access by tenant.

CREATE TABLE IF NOT EXISTS sync_schedules (
    id           VARCHAR(26) PRIMARY KEY,
    tenant_id    VARCHAR(26) NOT NULL,
    source_id    VARCHAR(26) NOT NULL,
    cron_expr    VARCHAR(64) NOT NULL,
    next_run_at  TIMESTAMPTZ NOT NULL,
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    last_run_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_sync_schedules_tenant_source
    ON sync_schedules (tenant_id, source_id);

CREATE INDEX IF NOT EXISTS idx_sync_schedules_due
    ON sync_schedules (next_run_at, enabled);

CREATE INDEX IF NOT EXISTS idx_sync_schedules_tenant
    ON sync_schedules (tenant_id);
