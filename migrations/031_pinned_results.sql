-- 031_pinned_results.sql — Round-7 Task 17.
-- Operator-pinned retrieval results. Each row maps a query pattern
-- (exact match in this iteration) to a chunk_id and a position in
-- the merged result list. The retrieval handler injects pinned
-- chunks at their configured position after policy filtering and
-- before diversity / topK truncation.

CREATE TABLE IF NOT EXISTS pinned_retrieval_results (
    id             CHAR(26)     NOT NULL,
    tenant_id      CHAR(26)     NOT NULL,
    query_pattern  TEXT         NOT NULL,
    chunk_id       VARCHAR(128) NOT NULL,
    position       INTEGER      NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_pinned_results_tenant_pattern
    ON pinned_retrieval_results (tenant_id, query_pattern);
