-- 043_connector_sync_cursors.down.sql — Round-20/21 Task 14.
--
-- Drops the connector_sync_cursors table introduced in 043.
-- Rolling back this migration loses the per-(source, namespace)
-- cursor history; subsequent DeltaSync calls will re-bootstrap
-- from the high-water mark and may miss in-flight changes that
-- occurred since the last cursor advance.

BEGIN;

DROP INDEX IF EXISTS idx_connector_sync_cursors_stale;
DROP INDEX IF EXISTS idx_connector_sync_cursors_tenant_updated;
DROP TABLE IF EXISTS connector_sync_cursors;

COMMIT;
