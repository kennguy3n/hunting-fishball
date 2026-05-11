-- 026_credential_valid.sql — Round-7 Task 7.
-- Adds a `credential_valid` boolean + `credential_checked_at` timestamp
-- to source_health so the CredentialHealthWorker can record the
-- outcome of `connector.Validate()` runs alongside the existing
-- sync-health signals.

ALTER TABLE source_health
    ADD COLUMN IF NOT EXISTS credential_valid BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE source_health
    ADD COLUMN IF NOT EXISTS credential_checked_at TIMESTAMPTZ;
ALTER TABLE source_health
    ADD COLUMN IF NOT EXISTS credential_error TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_source_health_credential_valid
    ON source_health (tenant_id, credential_valid);
