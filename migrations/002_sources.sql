-- 002_sources.sql
-- Sources table for hunting-fishball. One row per (tenant, connector
-- instance). Status lifecycle: pending → active ↔ paused → removing →
-- removed. The forget worker (internal/admin/forget_worker.go) is the
-- only writer that flips removing → removed.
--
-- Reads are always tenant-scoped (`tenant_id = $1` is mandatory).
-- Indexes lead with tenant_id and end with id DESC so the admin
-- portal's default "show me my tenant's sources, newest first" view
-- is a single index scan.
--
-- `config` holds the connector-specific settings (e.g. SharePoint
-- site URL, Slack workspace ID) and credential ciphertext as JSONB.
-- `scopes` is a JSONB array of namespace filters — empty array means
-- "all namespaces".

CREATE TABLE IF NOT EXISTS sources (
    id              CHAR(26)    PRIMARY KEY,
    tenant_id       CHAR(26)    NOT NULL,
    connector_type  VARCHAR(64) NOT NULL,
    config          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    scopes          JSONB       NOT NULL DEFAULT '[]'::jsonb,
    status          VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Composite index: tenant + newest-first list. Covers the default
-- admin view.
CREATE INDEX IF NOT EXISTS idx_sources_tenant_id
    ON sources (tenant_id, id DESC);

-- Composite index: tenant + status filter. Used by the admin portal
-- "show me my failing sources" view and the forget worker's pickup
-- query (`status = 'removing'`).
CREATE INDEX IF NOT EXISTS idx_sources_tenant_status
    ON sources (tenant_id, status);
