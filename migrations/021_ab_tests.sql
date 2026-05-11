-- 021_ab_tests.sql — Round-6 Task 10.
-- Persisted retrieval A/B experiments. Both arms (control,
-- variant) are stored as JSON documents so the schema is open
-- to future config knobs without churning the migration history.

CREATE TABLE IF NOT EXISTS retrieval_ab_tests (
    tenant_id              CHAR(26)    NOT NULL,
    experiment_name        VARCHAR(64) NOT NULL,
    status                 VARCHAR(16) NOT NULL DEFAULT 'draft',
    traffic_split_percent  SMALLINT    NOT NULL DEFAULT 0,
    control_config         JSONB       NOT NULL DEFAULT '{}',
    variant_config         JSONB       NOT NULL DEFAULT '{}',
    start_at               TIMESTAMPTZ,
    end_at                 TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, experiment_name)
);

CREATE INDEX IF NOT EXISTS idx_retrieval_ab_tests_active
    ON retrieval_ab_tests (tenant_id, status);
