-- 010_retention_policy.sql
-- Phase 8 / Task 6: tenant- and source-level retention rules.
--
-- The PROPOSAL.md §5 contract: "Documents inherit the source's
-- retention rules by default; tenants can layer additional retention
-- policy on top." This table stores both layers; the pipeline
-- retention worker resolves the effective MaxAgeDays at sweep time
-- (more-restrictive scope wins).
--
-- Scope semantics:
--   tenant      → applies to every chunk owned by tenant_id
--   source      → applies to every chunk where (tenant_id, source_id) match
--   namespace   → applies to every chunk where (tenant_id, source_id, namespace_id) match
--
-- The (tenant_id, scope, scope_value) tuple is unique so a second
-- write of the same scope replaces the prior rule.

CREATE TABLE IF NOT EXISTS retention_policies (
    id              CHAR(26)    NOT NULL PRIMARY KEY,
    tenant_id       CHAR(26)    NOT NULL,
    scope           VARCHAR(16) NOT NULL,        -- 'tenant' | 'source' | 'namespace'
    scope_value     VARCHAR(256) NOT NULL DEFAULT '',
    max_age_days    INT         NOT NULL,        -- 0 = no expiry (rule disabled)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT retention_scope_check CHECK (scope IN ('tenant','source','namespace')),
    CONSTRAINT retention_unique_scope UNIQUE (tenant_id, scope, scope_value)
);

CREATE INDEX IF NOT EXISTS idx_retention_tenant ON retention_policies (tenant_id);
