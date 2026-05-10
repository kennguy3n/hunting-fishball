-- Rollback for 011_varchar_ids.sql.
--
-- Reverts the VARCHAR(26) policy/audit ID columns back to
-- CHAR(26). NOTE: this re-introduces the PostgreSQL blank-padding
-- footgun that the forward migration was created to fix. Only
-- apply if a downstream system depends on the CHAR contract;
-- otherwise leave the schema at VARCHAR.
ALTER TABLE policy_drafts    ALTER COLUMN channel_id TYPE CHAR(26);
ALTER TABLE policy_acl_rules ALTER COLUMN channel_id TYPE CHAR(26);
ALTER TABLE policy_acl_rules ALTER COLUMN source_id  TYPE CHAR(26);
ALTER TABLE channel_policies ALTER COLUMN channel_id TYPE CHAR(26);
ALTER TABLE recipient_policies ALTER COLUMN channel_id TYPE CHAR(26);
ALTER TABLE shards           ALTER COLUMN channel_id TYPE CHAR(26);
ALTER TABLE audit_logs       ALTER COLUMN actor_id    TYPE CHAR(26);
ALTER TABLE audit_logs       ALTER COLUMN resource_id TYPE CHAR(26);
