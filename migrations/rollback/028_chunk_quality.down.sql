-- Rollback for 028_chunk_quality.sql.
DROP INDEX IF EXISTS idx_chunk_quality_tenant_source;
DROP TABLE IF EXISTS chunk_quality;
