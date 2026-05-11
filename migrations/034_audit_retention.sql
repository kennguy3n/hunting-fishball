-- 034_audit_retention.sql
-- Round-12 Task 17 — audit_logs retention sweep support.
--
-- The retention sweeper (internal/admin/audit_retention.go) deletes
-- rows whose created_at is older than the configured retention
-- window (default 90 days). The original 001_audit_log.sql does NOT
-- index audit_logs.created_at directly — its composite indexes lead
-- with tenant_id — so a global "WHERE created_at < $cutoff" query
-- would scan every partition.
--
-- This migration adds a partial-coverage index dedicated to the
-- sweeper: BRIN is cheap on append-only timestamp columns and
-- keeps writes fast, but BRIN is Postgres-only. We use a B-tree
-- index for portability with SQLite (used by tests and the
-- migration dry-run gate); on Postgres in production the planner
-- already chooses this btree for time-range deletes.

CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at
    ON audit_logs (created_at);
