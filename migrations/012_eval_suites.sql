-- 007_eval_suites.sql
-- Phase 6: persisted retrieval-evaluation corpora.
--
-- Each row is a labelled set of (query, expected_chunk_ids) tuples
-- that an admin can replay against the live retrieval handler to
-- score Precision@K, Recall@K, MRR, and NDCG. The cases are stored
-- as a single JSONB blob so the admin portal can edit them as a
-- unit; (tenant_id, name) is unique so name re-use becomes
-- edit-in-place rather than dup-rows.
--
-- Run via:  POST /v1/admin/eval/run {"suite_name": "<name>"}
-- See:      docs/ARCHITECTURE.md §4.5 "Evaluation harness".

CREATE TABLE IF NOT EXISTS eval_suites (
    id           VARCHAR(26) NOT NULL PRIMARY KEY,
    tenant_id    VARCHAR(26) NOT NULL,
    name         VARCHAR(128) NOT NULL,
    corpus       JSONB NOT NULL DEFAULT '[]'::jsonb,
    default_k    INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_eval_suites_tenant_name
    ON eval_suites (tenant_id, name);

CREATE INDEX IF NOT EXISTS idx_eval_suites_tenant
    ON eval_suites (tenant_id);
