-- 011_varchar_ids.sql
-- PostgreSQL CHAR(N) blank-pads stored values out to N characters and
-- returns them padded on read, so columns that intentionally store
-- the empty string as a wildcard sentinel (e.g. policy_acl_rules.
-- channel_id == '' meaning "tenant-wide") came back as 26 spaces and
-- broke equality checks in the Go layer (`r.SourceID != ""` was true,
-- so the rule never matched the chunk).
--
-- VARCHAR(N) does NOT pad, so converting in place fixes the problem
-- without changing the application contract. Existing rows are
-- TRIM()'d so any pre-conversion empties stop carrying their phantom
-- spaces around.

ALTER TABLE policy_drafts    ALTER COLUMN channel_id TYPE VARCHAR(26);
ALTER TABLE policy_acl_rules ALTER COLUMN channel_id TYPE VARCHAR(26);
ALTER TABLE policy_acl_rules ALTER COLUMN source_id  TYPE VARCHAR(26);
ALTER TABLE channel_policies ALTER COLUMN channel_id TYPE VARCHAR(26);
ALTER TABLE recipient_policies ALTER COLUMN channel_id TYPE VARCHAR(26);
ALTER TABLE shards           ALTER COLUMN channel_id TYPE VARCHAR(26);
ALTER TABLE audit_logs       ALTER COLUMN actor_id    TYPE VARCHAR(26);
ALTER TABLE audit_logs       ALTER COLUMN resource_id TYPE VARCHAR(26);

UPDATE policy_drafts      SET channel_id  = TRIM(channel_id)  WHERE channel_id  IS NOT NULL;
UPDATE policy_acl_rules   SET channel_id  = TRIM(channel_id),
                              source_id   = TRIM(source_id);
UPDATE channel_policies   SET channel_id  = TRIM(channel_id);
UPDATE recipient_policies SET channel_id  = TRIM(channel_id);
UPDATE shards             SET channel_id  = TRIM(channel_id)  WHERE channel_id  IS NOT NULL;
UPDATE audit_logs         SET actor_id    = TRIM(actor_id)    WHERE actor_id    IS NOT NULL;
UPDATE audit_logs         SET resource_id = TRIM(resource_id) WHERE resource_id IS NOT NULL;
