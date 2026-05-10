-- 006_shards.sql
-- Phase 5 server-side: per-(tenant, user, channel, privacy_mode)
-- shard manifests. The shard is the unit the on-device knowledge core
-- syncs from the server: a serialised list of chunk IDs + embeddings
-- that satisfies the sender's policy at the time the shard was
-- generated.
--
-- The manifest row is the metadata; the chunk IDs themselves live in
-- a sibling shard_chunks table so the (tenant, channel, version) →
-- chunk-ids fan-out is index-friendly. Both tables are tenant-scoped;
-- every read query MUST start with `tenant_id = ?`.
--
-- Status transitions (manifest.status):
--
--   pending   → ready      (generator finished; chunk IDs persisted)
--   pending   → failed     (generator gave up after retries)
--   ready     → superseded (a newer version replaced this manifest)

CREATE TABLE IF NOT EXISTS shards (
    id            CHAR(26)    NOT NULL PRIMARY KEY,
    tenant_id     CHAR(26)    NOT NULL,
    user_id       VARCHAR(64),                                -- empty = tenant-wide shard
    channel_id    CHAR(26),                                   -- empty = tenant-wide
    privacy_mode  VARCHAR(32) NOT NULL,
    shard_version BIGINT      NOT NULL DEFAULT 1,
    chunks_count  INT         NOT NULL DEFAULT 0,
    status        VARCHAR(16) NOT NULL DEFAULT 'pending',     -- pending|ready|failed|superseded
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, user_id, channel_id, privacy_mode, shard_version)
);

CREATE INDEX IF NOT EXISTS idx_shards_tenant_user_channel
    ON shards (tenant_id, user_id, channel_id);

CREATE INDEX IF NOT EXISTS idx_shards_tenant_status
    ON shards (tenant_id, status);

-- Per-shard chunk membership. A delta sync between client_version and
-- server_version is computed by joining the two version's chunk_id
-- rows under the same (tenant, user, channel, privacy_mode) scope.
CREATE TABLE IF NOT EXISTS shard_chunks (
    shard_id   CHAR(26)     NOT NULL,
    chunk_id   VARCHAR(128) NOT NULL,
    tenant_id  CHAR(26)     NOT NULL,
    PRIMARY KEY (shard_id, chunk_id)
);

CREATE INDEX IF NOT EXISTS idx_shard_chunks_tenant_shard
    ON shard_chunks (tenant_id, shard_id);

-- Tenant lifecycle marker for cryptographic forgetting (Task 4).
-- A row exists for tenants whose key destruction has been requested.
CREATE TABLE IF NOT EXISTS tenant_lifecycle (
    tenant_id    CHAR(26)    NOT NULL PRIMARY KEY,
    state        VARCHAR(32) NOT NULL,                        -- pending_deletion|deleted
    requested_by TEXT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ
);
