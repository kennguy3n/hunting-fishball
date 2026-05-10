-- Rollback for 005_policy_drafts.sql.
DROP INDEX IF EXISTS idx_policy_drafts_tenant;
DROP TABLE IF EXISTS policy_drafts;
