-- 017_policy_versions.sql — Round-5 Task 15.
--
-- policy_versions records every promotion / rejection of a policy
-- draft so admins can see the lineage of a tenant's policy and
-- (via /v1/admin/policy/rollback/:version_id) recreate a draft
-- from any past snapshot. The table is append-only — there is no
-- updated_at column.
CREATE TABLE IF NOT EXISTS policy_versions (
    id           CHAR(26) NOT NULL,
    tenant_id    CHAR(26) NOT NULL,
    channel_id   VARCHAR(128) NOT NULL DEFAULT '',
    draft_id     CHAR(26) NOT NULL,
    action       VARCHAR(32) NOT NULL,
    actor_id     VARCHAR(64) NOT NULL DEFAULT '',
    snapshot     JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_policy_versions_tenant
    ON policy_versions (tenant_id, id);

CREATE INDEX IF NOT EXISTS idx_policy_versions_tenant_channel
    ON policy_versions (tenant_id, channel_id, id);
