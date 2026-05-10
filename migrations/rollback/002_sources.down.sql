-- Rollback for 002_sources.sql.
--
-- Drops the sources table and its supporting indexes. Active
-- syncs MUST be paused before applying — the ingest pipeline
-- joins against sources on every event.
DROP INDEX IF EXISTS idx_sources_tenant_status;
DROP INDEX IF EXISTS idx_sources_tenant_id;
DROP TABLE IF EXISTS sources;
