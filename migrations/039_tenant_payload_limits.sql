-- Round-14 Task 8: per-tenant request-body size override.
--
-- The global limiter (Round-13 Task 11) caps every tenant at the
-- value of CONTEXT_ENGINE_MAX_REQUEST_BODY_BYTES. A handful of
-- power tenants legitimately ingest larger batch payloads, so we
-- carry a per-tenant override. NULL / missing row falls back to
-- the global default.
--
-- Round-14 Task 14 reuses the same migration slot range: the DLQ
-- categorization migration is 040 to keep ordering monotonic.
CREATE TABLE IF NOT EXISTS tenant_payload_limits (
    tenant_id      CHAR(26)     NOT NULL PRIMARY KEY,
    max_bytes      BIGINT       NOT NULL,
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- The store reads by tenant_id only, but operators often want to
-- list all overrides sorted by recency for audit. The pk already
-- covers the read path; this index is a comfort for the audit
-- query.
CREATE INDEX IF NOT EXISTS idx_tenant_payload_limits_updated_at
    ON tenant_payload_limits (updated_at DESC);
