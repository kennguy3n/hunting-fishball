-- 042_document_content_type.down.sql — Round-19 Task 22.
--
-- Drops the content_type tag added in 042. Intentionally
-- destructive: the column carries chunk + document classification
-- data, so a rollback after Stage 2 has started populating real
-- multimodal values loses that data. The forward migration
-- back-fills every existing row to 'text', so the rollback only
-- discards information added after migration 042 ran.

BEGIN;

DROP INDEX IF EXISTS idx_chunks_content_type;
DROP INDEX IF EXISTS idx_documents_content_type;

ALTER TABLE chunks DROP COLUMN IF EXISTS content_type;
ALTER TABLE documents DROP COLUMN IF EXISTS content_type;

COMMIT;
