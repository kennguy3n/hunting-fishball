-- Rollback for 006_shards.sql.
--
-- Drops the per-tenant logical sharding tables. The pipeline's
-- shard hashing falls back to a no-op when these tables are
-- missing, so rollback is non-destructive (chunks just land in
-- the default shard).
DROP INDEX IF EXISTS idx_shard_chunks_tenant_shard;
DROP INDEX IF EXISTS idx_shards_tenant_status;
DROP INDEX IF EXISTS idx_shards_tenant_user_channel;
DROP TABLE IF EXISTS tenant_lifecycle;
DROP TABLE IF EXISTS shard_chunks;
DROP TABLE IF EXISTS shards;
