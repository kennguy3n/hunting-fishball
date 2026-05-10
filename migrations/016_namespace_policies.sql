-- 016_namespace_policies.sql — Round-5 Task 8.
--
-- namespace_policies layers a third strictness level under
-- tenant_policies + channel_policies. EffectiveModeForNamespace
-- picks the strictest of (tenant, channel, namespace), so a
-- namespace marked `no-ai` collapses retrieval for that namespace
-- regardless of the channel's `remote` setting.
--
-- A "namespace" here is the same string the connector layer uses
-- to label sub-folders / sub-projects within a source (e.g.
-- "engineering" inside a Confluence space, or "/HR" inside a Box
-- folder tree). The (tenant_id, namespace_id) tuple is the
-- primary key — namespaces are scoped by tenant, not by channel,
-- so an admin only has to pin a namespace once and every channel
-- inherits the override.

CREATE TABLE IF NOT EXISTS namespace_policies (
    tenant_id    VARCHAR(26)  NOT NULL,
    namespace_id VARCHAR(128) NOT NULL,
    privacy_mode VARCHAR(32)  NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, namespace_id)
);

-- Reads are always per-tenant; the index speeds up the per-tenant
-- snapshot the live resolver builds.
CREATE INDEX IF NOT EXISTS idx_namespace_policies_tenant
    ON namespace_policies (tenant_id);
