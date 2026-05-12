-- 037_api_keys.sql — Round-13 Task 10.
--
-- Stores per-tenant API keys with status + grace-period support.
-- The POST /v1/admin/tenants/:tenant_id/rotate-api-key endpoint
-- inserts a fresh row, returns the cleartext key once, and marks
-- any prior active row with a deactivated_at + grace_until so
-- clients have a configurable window
-- (CONTEXT_ENGINE_API_KEY_GRACE_PERIOD, default 24h) to roll over
-- without 401s.
--
-- We deliberately store the SHA-256 hash of the key rather than
-- the raw value so a future database breach does not leak live
-- credentials. The application never reads the cleartext after
-- the initial rotation response.
--
-- Statuses (enforced by the application layer; column is plain
-- varchar to keep migrations cheap):
--   active   — accepted on every request.
--   grace    — accepted only while now() <= grace_until.
--   revoked  — rejected unconditionally.

CREATE TABLE IF NOT EXISTS api_keys (
    id              CHAR(26) PRIMARY KEY,
    tenant_id       CHAR(26) NOT NULL,
    key_hash        VARCHAR(64) NOT NULL,
    status          VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deactivated_at  TIMESTAMPTZ,
    grace_until     TIMESTAMPTZ,
    UNIQUE (key_hash)
);

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_status
    ON api_keys (tenant_id, status);
