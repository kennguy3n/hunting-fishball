-- 018_feedback_events.sql — Round-5 Task 16.
--
-- feedback_events captures every relevance signal an admin or
-- end-user gives back to the retrieval engine. The eval runner
-- (PR #13) consumes these as ground-truth alongside the curated
-- golden corpus from Task 1, which lets the precision/MRR
-- regression gate adapt to real-world traffic.
CREATE TABLE IF NOT EXISTS feedback_events (
    id          CHAR(26) NOT NULL,
    tenant_id   CHAR(26) NOT NULL,
    query_id    VARCHAR(64) NOT NULL,
    chunk_id    VARCHAR(128) NOT NULL,
    user_id     VARCHAR(64) NOT NULL DEFAULT '',
    relevant    BOOLEAN NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_feedback_events_tenant_query
    ON feedback_events (tenant_id, query_id);

CREATE INDEX IF NOT EXISTS idx_feedback_events_tenant_chunk
    ON feedback_events (tenant_id, chunk_id);
