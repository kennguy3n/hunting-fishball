-- Rollback for 004_policy.sql.
--
-- Drops the live policy tables (tenant_policies, channel_policies,
-- policy_acl_rules, recipient_policies). Retrieval falls back to
-- "default-allow" when the resolver finds no rows, so a rollback
-- here softens enforcement to "private + tenant-only" until the
-- forward migration is reapplied. See PROPOSAL §6.
DROP INDEX IF EXISTS idx_recipient_policies_tenant_channel;
DROP INDEX IF EXISTS idx_policy_acl_rules_tenant_channel;
DROP INDEX IF EXISTS idx_channel_policies_tenant;
DROP TABLE IF EXISTS recipient_policies;
DROP TABLE IF EXISTS policy_acl_rules;
DROP TABLE IF EXISTS channel_policies;
DROP TABLE IF EXISTS tenant_policies;
