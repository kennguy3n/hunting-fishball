-- Rollback for 019_source_embedding_config.sql.
DROP INDEX IF EXISTS idx_source_embedding_config_tenant;
DROP TABLE IF EXISTS source_embedding_config;
