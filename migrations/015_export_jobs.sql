-- 015_export_jobs.sql — Round-5 Task 7.
--
-- export_jobs tracks GDPR data-subject export requests. The
-- admin API queues a job via POST /v1/admin/tenants/:id/export
-- which kicks off a background worker that walks the tenant's
-- source metadata, policy snapshots, audit log entries, and
-- chunk metadata (NOT raw content or embeddings — only metadata).
-- The worker writes a JSON archive and the admin polls
-- GET /v1/admin/tenants/:id/export/:job_id for status and the
-- download URL.
--
-- Lifecycle: pending -> running -> succeeded | failed
-- The download_url is set when status becomes succeeded; the
-- error column is set when status becomes failed. Everything
-- else is request metadata captured at job creation time.

CREATE TABLE IF NOT EXISTS export_jobs (
    id           VARCHAR(26)  NOT NULL,
    tenant_id    VARCHAR(26)  NOT NULL,
    actor_id     VARCHAR(64)  NOT NULL DEFAULT '',
    status       VARCHAR(16)  NOT NULL DEFAULT 'pending',
    download_url TEXT         NOT NULL DEFAULT '',
    error        TEXT         NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (id)
);

-- Tenant scans walk the per-tenant export history.
CREATE INDEX IF NOT EXISTS idx_export_jobs_tenant
    ON export_jobs (tenant_id, created_at DESC);

-- The worker polls the next pending job.
CREATE INDEX IF NOT EXISTS idx_export_jobs_status
    ON export_jobs (status, created_at)
    WHERE status IN ('pending', 'running');
