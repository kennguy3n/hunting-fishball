-- Rollback for 020_chunk_acl.sql.
DROP INDEX IF EXISTS idx_chunk_acl_chunk;
DROP INDEX IF EXISTS idx_chunk_acl_tenant;
DROP TABLE IF EXISTS chunk_acl;
