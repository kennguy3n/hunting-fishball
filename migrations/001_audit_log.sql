-- 001_audit_log.sql
-- Audit log table for hunting-fishball. Append-only: no updated_at, no
-- deleted_at. IDs are ULIDs stored as char(26) so ORDER BY id DESC is
-- equivalent to "newest first" without a separate created_at index.
--
-- Reads are always tenant-scoped (`tenant_id = $1` is mandatory), so the
-- composite indexes lead with tenant_id and end with id DESC to cover
-- filter-and-sort in a single index scan.

CREATE TABLE IF NOT EXISTS audit_logs (
    id              CHAR(26)    PRIMARY KEY,
    tenant_id       CHAR(26)    NOT NULL,
    actor_id        CHAR(26),
    action          VARCHAR(64) NOT NULL,
    resource_type   VARCHAR(64) NOT NULL,
    resource_id     CHAR(26),
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    trace_id        VARCHAR(64),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ
);

-- Composite index: tenant + newest-first list. Covers the default
-- "show me my tenant's recent activity" query.
CREATE INDEX IF NOT EXISTS idx_audit_tenant_created
    ON audit_logs (tenant_id, id DESC);

-- Composite index: tenant + actor (for "who did what" admin views).
CREATE INDEX IF NOT EXISTS idx_audit_tenant_actor
    ON audit_logs (tenant_id, actor_id, id DESC);

-- Composite index: tenant + action filter.
CREATE INDEX IF NOT EXISTS idx_audit_tenant_action
    ON audit_logs (tenant_id, action, id DESC);

-- Partial index: rows the outbox poller still needs to publish to Kafka.
-- Once published_at is set the row drops out of the index, keeping it
-- bounded by the publishing lag rather than the table size.
CREATE INDEX IF NOT EXISTS idx_audit_unpublished
    ON audit_logs (id)
    WHERE published_at IS NULL;
