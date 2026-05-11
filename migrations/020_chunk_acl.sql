-- 020_chunk_acl.sql — Round-6 Task 6.
-- Per-chunk ACL tags evaluated by the retrieval handler after the
-- source-level AllowDenyList has approved a chunk. Either chunk_id
-- (exact match) or tag_prefix (prefix match against the chunk's tag
-- set) targets a chunk; decision is allow/deny with deny-wins
-- semantics.

CREATE TABLE IF NOT EXISTS chunk_acl (
    id           BIGSERIAL    PRIMARY KEY,
    tenant_id    CHAR(26)     NOT NULL,
    channel_id   CHAR(26)     NOT NULL DEFAULT '',
    chunk_id     VARCHAR(64)  NOT NULL DEFAULT '',
    tag_prefix   VARCHAR(128) NOT NULL DEFAULT '',
    decision     VARCHAR(8)   NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_chunk_acl_tenant
    ON chunk_acl (tenant_id, channel_id);
CREATE INDEX IF NOT EXISTS idx_chunk_acl_chunk
    ON chunk_acl (tenant_id, chunk_id);
