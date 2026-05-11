-- 032_synonyms.sql — Round-9 Task 4.
-- Per-tenant synonym map used by the retrieval query expander.
-- Each (tenant_id, term) row carries the JSONB array of synonyms
-- that the expander appends to the user's query. The expander
-- writes the full synonym map atomically (one transaction wipes
-- the prior rows and inserts the new ones), so the rowwise model
-- below keeps reads cheap without coupling the writer to a JSONB
-- merge operation.

CREATE TABLE IF NOT EXISTS retrieval_synonyms (
    tenant_id   CHAR(26)    NOT NULL,
    term        VARCHAR(64) NOT NULL,
    synonyms    JSONB       NOT NULL DEFAULT '[]',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, term)
);

CREATE INDEX IF NOT EXISTS idx_retrieval_synonyms_tenant
    ON retrieval_synonyms (tenant_id);
