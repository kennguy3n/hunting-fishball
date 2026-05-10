-- Rollback for 021_ab_tests.sql.
DROP INDEX IF EXISTS idx_retrieval_ab_tests_active;
DROP TABLE IF EXISTS retrieval_ab_tests;
