-- Round-18 Task 11 rollback.
DROP INDEX IF EXISTS idx_chunk_embedding_model;
DROP INDEX IF EXISTS idx_chunk_embedding_stale;
DROP TABLE IF EXISTS chunk_embedding_version;
