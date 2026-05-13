-- 043_connector_sync_cursors.sql — Round-20/21 Task 14.
--
-- DeltaSync cursor normalization. Prior to Round 20, each
-- connector persisted its DeltaSync cursor inside the source
-- config blob (sources.config_json -> "cursor"). That worked
-- but had three problems:
--
--   1. Cursors are per (source, namespace) — a single source
--      (e.g. a Slack workspace) syncs many namespaces (channels)
--      and the JSON blob serialized them all together,
--      forcing the ingester to write the whole blob on every
--      cursor advance and risking lost updates under contention.
--   2. The cursor blob mixed mutable runtime state with the
--      tenant's configuration, so a `GET /v1/admin/sources/:id`
--      response leaked internal cursors back to the customer
--      unless the API explicitly stripped them.
--   3. Cursor history (last-N values) was impossible to keep
--      without bloating the config blob.
--
-- This migration introduces the dedicated `connector_sync_cursors`
-- table, keyed on (tenant_id, source_id, namespace_id). The
-- ingester writes here on every DeltaSync advance via an UPSERT
-- (CONFLICT (tenant_id, source_id, namespace_id) DO UPDATE).
-- The source config blob remains the source of truth for the
-- one-time bootstrap setting.

BEGIN;

CREATE TABLE IF NOT EXISTS connector_sync_cursors (
    tenant_id     CHAR(26)      NOT NULL,
    source_id     CHAR(26)      NOT NULL,
    -- namespace_id is the connector-defined namespace identifier
    -- (Slack channel ID, Notion database ID, Box folder ID, etc.).
    -- Empty string represents the "default" / single-namespace
    -- case (e.g. RSS feeds, Sitemap connectors).
    namespace_id  VARCHAR(256)  NOT NULL,
    -- Latest cursor value as opaque text. Different connectors
    -- use different cursor shapes — Microsoft Graph delta tokens,
    -- unix timestamps, ISO 8601 timestamps, base64 page tokens,
    -- BookStack last-known modified time, etc.
    cursor        TEXT          NOT NULL,
    -- Wall-clock time the cursor was written. Used by the
    -- per-source sync-lag SLO calculations and surfaced on the
    -- connector health dashboard.
    updated_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    -- Optional: error from the last DeltaSync call. NULL means
    -- the last sync succeeded.
    last_error    TEXT,
    PRIMARY KEY (tenant_id, source_id, namespace_id)
);

-- Index for the operator-facing dashboard query: "show me every
-- source's most-recent cursor advance per tenant".
CREATE INDEX IF NOT EXISTS idx_connector_sync_cursors_tenant_updated
    ON connector_sync_cursors (tenant_id, updated_at DESC);

-- Index for the auto-pauser sweep: "find sources whose last
-- cursor advance was older than the pause threshold".
CREATE INDEX IF NOT EXISTS idx_connector_sync_cursors_stale
    ON connector_sync_cursors (updated_at)
    WHERE last_error IS NOT NULL;

COMMIT;
