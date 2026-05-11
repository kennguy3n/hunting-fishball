-- Rollback for 026_credential_valid.sql.
DROP INDEX IF EXISTS idx_source_health_credential_valid;
ALTER TABLE source_health DROP COLUMN IF EXISTS credential_error;
ALTER TABLE source_health DROP COLUMN IF EXISTS credential_checked_at;
ALTER TABLE source_health DROP COLUMN IF EXISTS credential_valid;
