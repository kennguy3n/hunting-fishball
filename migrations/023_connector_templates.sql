-- 023_connector_templates.sql — Round-6 Task 16.

CREATE TABLE IF NOT EXISTS connector_templates (
    id              CHAR(26)    NOT NULL,
    tenant_id       CHAR(26)    NOT NULL,
    connector_type  VARCHAR(64) NOT NULL,
    default_config  JSONB       NOT NULL DEFAULT '{}',
    description     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_connector_templates_tenant
    ON connector_templates (tenant_id, connector_type);
