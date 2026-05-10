-- Rollback for 007_channel_deny_local.sql.
--
-- Drops the deny_local_retrieval column. Active channel policies
-- with the column set to TRUE will revert to "allow local" after
-- this rollback. Caller is responsible for snapshotting the
-- column values first if they are needed downstream.
ALTER TABLE channel_policies DROP COLUMN IF EXISTS deny_local_retrieval;
