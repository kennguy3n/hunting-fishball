-- 005_policy_drafts.sql
-- Phase 4 policy simulator: draft policy store. Drafts are isolated
-- from the live policy tables (tenant_policies, channel_policies,
-- policy_acl_rules, recipient_policies). The retrieval handler's
-- PolicyResolver MUST NOT read from this table — drafts are an
-- admin-portal-only construct until they are explicitly promoted by
-- an audited workflow (see internal/policy/promotion.go).
--
-- A draft carries the entire proposed PolicySnapshot in `payload`
-- (JSONB) so the simulator can what-if it without joining across
-- the live tables. Status transitions:
--
--   draft     → promoted    (PromoteDraft, atomic w/ live writes)
--   draft     → rejected    (RejectDraft, no live write)
--   promoted  → (terminal)
--   rejected  → (terminal)

CREATE TABLE IF NOT EXISTS policy_drafts (
    id           CHAR(26)    NOT NULL PRIMARY KEY,
    tenant_id    CHAR(26)    NOT NULL,
    channel_id   CHAR(26),                                  -- NULL = tenant-wide draft
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,  -- serialised PolicySnapshot
    status       VARCHAR(16) NOT NULL DEFAULT 'draft',      -- draft|promoted|rejected
    created_by   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    promoted_at  TIMESTAMPTZ,
    promoted_by  TEXT,
    reject_reason TEXT
);

-- Listing drafts for a tenant in the admin portal is the dominant
-- read pattern; partition the index on (tenant_id, status) so the
-- "show me everything still in draft" query stays in the index.
CREATE INDEX IF NOT EXISTS idx_policy_drafts_tenant
    ON policy_drafts (tenant_id, status);
