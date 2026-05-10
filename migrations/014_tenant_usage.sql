-- 014_tenant_usage.sql — Round-4 Task 17.
--
-- tenant_usage stores a per-tenant + per-day rollup of metering
-- counters. The metering layer increments rows here on every API
-- call, ingestion event, and chunk store. Operators query this
-- table via GET /v1/admin/tenants/:id/usage to see daily volume
-- and produce SaaS billing rollups.
--
-- The rollup grain is (tenant_id, day, metric). Metric is a short
-- enum: api_retrieve, api_admin, ingest_doc, chunk_count. Adding
-- a new metric is a no-schema change — the column is varchar.

CREATE TABLE IF NOT EXISTS tenant_usage (
    tenant_id  VARCHAR(26) NOT NULL,
    day        DATE        NOT NULL,
    metric     VARCHAR(64) NOT NULL,
    count      BIGINT      NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, day, metric)
);

-- Range scans for the GET ...?from=&to= endpoint walk
-- (tenant_id, day) — primary key already covers it.
-- Cross-tenant aggregations for SaaS reporting use day prefix.
CREATE INDEX IF NOT EXISTS idx_tenant_usage_day
    ON tenant_usage (day);
