-- 007_channel_deny_local.sql
-- Phase 5/6 wiring: per-channel toggle that forbids on-device shard
-- retrieval. The contract documented in
-- `docs/contracts/local-first-retrieval.md` defines channel-level
-- allow-local as default-true, so the column defaults to FALSE
-- (allow). Admins flip it to TRUE to force every query through the
-- remote API regardless of device tier or shard freshness.
--
-- See `policy.PolicySnapshot.DenyLocalRetrieval` (the in-memory
-- mirror) and `device_first.Decide` which reads the inverted value
-- through `DeviceFirstInputs.AllowLocalRetrieval` and emits the
-- `channel_disallowed` reason when the flag is set.

ALTER TABLE channel_policies
    ADD COLUMN IF NOT EXISTS deny_local_retrieval BOOLEAN NOT NULL DEFAULT FALSE;
