-- 008_tenant_status.sql
-- Phase 5/6: tenant lifecycle status used by the admin tenant
-- deletion workflow (see internal/admin/tenant_delete.go). The
-- existing tenant_lifecycle table (migrations/006_shards.sql) is
-- still the authoritative source of truth for the cryptographic-
-- forget orchestrator; this migration adds a denormalised
-- tenant_status column on a dedicated tenant directory table so
-- admin/list/management surfaces can show the current state without
-- joining the lifecycle table on every read.
--
-- States:
--   active             — default; tenant operational
--   pending_deletion   — admin DELETE issued; sweep in progress
--   deleted            — sweep completed; DEKs destroyed
--
-- The shard.Forget orchestrator transitions the tenant_lifecycle
-- row through pending_deletion → deleted; the admin TenantDeleter
-- mirrors those transitions onto this table so the admin portal
-- has a single, fast lookup column.

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id     CHAR(26)    NOT NULL PRIMARY KEY,
    tenant_status VARCHAR(32) NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_tenants_status
    ON tenants (tenant_status);
