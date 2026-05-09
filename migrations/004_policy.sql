-- 004_policy.sql
-- Phase 4 policy framework: privacy modes, ACL allow/deny rules,
-- and recipient policies. All tables are tenant-scoped; reads must
-- always lead with `tenant_id = ?`.
--
-- The retrieval handler's policy filter consults these tables on
-- every query. Edits emit `policy.edited` audit events so backend
-- replicas can invalidate any in-process cache.

-- Per-tenant default privacy mode. Channels inherit when no
-- channel_policies row exists.
CREATE TABLE IF NOT EXISTS tenant_policies (
    tenant_id     CHAR(26)    NOT NULL PRIMARY KEY,
    privacy_mode  VARCHAR(32) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-channel privacy mode override + recipient policy defaults.
-- The retrieval handler picks the STRICTER of (tenant, channel) via
-- policy.EffectiveMode.
CREATE TABLE IF NOT EXISTS channel_policies (
    tenant_id            CHAR(26)    NOT NULL,
    channel_id           CHAR(26)    NOT NULL,
    privacy_mode         VARCHAR(32) NOT NULL,
    recipient_default    VARCHAR(8)  NOT NULL DEFAULT 'allow', -- allow|deny
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, channel_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_policies_tenant
    ON channel_policies (tenant_id);

-- Per-(tenant, channel) ACL allow/deny rules. Empty source_id /
-- namespace_id / path_glob columns act as wildcards. The retrieval
-- handler evaluates these rules with deny-over-allow precedence
-- (see internal/policy/acl.go).
CREATE TABLE IF NOT EXISTS policy_acl_rules (
    id            CHAR(26)    NOT NULL PRIMARY KEY,
    tenant_id     CHAR(26)    NOT NULL,
    channel_id    CHAR(26),                                 -- NULL = tenant-wide
    source_id     CHAR(26),                                 -- NULL = any source
    namespace_id  VARCHAR(128),                             -- NULL = any namespace
    path_glob     VARCHAR(512),                             -- NULL = any path
    action        VARCHAR(8)  NOT NULL,                     -- allow|deny
    compute_tier  VARCHAR(32),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_policy_acl_rules_tenant_channel
    ON policy_acl_rules (tenant_id, channel_id);

-- Per-(tenant, channel, skill) recipient allow/deny. Empty skill_id
-- is the catch-all rule. DefaultAllow lives on channel_policies.
CREATE TABLE IF NOT EXISTS recipient_policies (
    id          CHAR(26)    NOT NULL PRIMARY KEY,
    tenant_id   CHAR(26)    NOT NULL,
    channel_id  CHAR(26)    NOT NULL,
    skill_id    VARCHAR(64),                                -- NULL = catch-all
    action      VARCHAR(8)  NOT NULL,                       -- allow|deny
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_recipient_policies_tenant_channel
    ON recipient_policies (tenant_id, channel_id);
