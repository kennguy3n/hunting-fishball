-- Rollback for 032_synonyms.sql.
DROP INDEX IF EXISTS idx_retrieval_synonyms_tenant;
DROP TABLE IF EXISTS retrieval_synonyms;
