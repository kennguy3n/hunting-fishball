-- 027_latency_budgets.sql — Round-7 Task 11.
-- Per-tenant retrieval latency budget. The retrieval handler reads
-- this row when no explicit max_latency_ms is set on the request;
-- budget violations are exported as the
-- context_engine_retrieval_budget_violations_total counter.

CREATE TABLE IF NOT EXISTS tenant_latency_budgets (
    tenant_id      CHAR(26)    NOT NULL,
    max_latency_ms INTEGER     NOT NULL DEFAULT 500,
    p95_target_ms  INTEGER     NOT NULL DEFAULT 500,
    notes          TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id)
);
