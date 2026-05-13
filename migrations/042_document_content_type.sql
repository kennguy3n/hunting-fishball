-- 042_document_content_type.sql — Round-19 Task 22.
--
-- Multi-modal preparation. Round-1..17 treated every ingested
-- document as text — the chunk store has no notion of
-- "image" vs "audio" vs "video" vs "text". This is the migration
-- that adds a coarse-grained content_type tag onto the chunks /
-- documents tables so Stage 2 (Parse) and the retrieval surface
-- can route per-modality.
--
-- Why a string and not an enum:
--   - Stage 2 will grow modality-specific parsers asynchronously
--     (Docling multimodal, Whisper, video frame-extractor). A
--     plain text column lets new modalities land without an
--     ALTER TYPE migration.
--
-- Why a default of 'text':
--   - Round-1..17 chunks are by definition text. Backfilling
--     them to 'text' lets the column be NOT NULL without a
--     separate fix-up worker.

BEGIN;

ALTER TABLE chunks
    ADD COLUMN IF NOT EXISTS content_type TEXT NOT NULL DEFAULT 'text';

CREATE INDEX IF NOT EXISTS idx_chunks_content_type
    ON chunks (content_type);

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS content_type TEXT NOT NULL DEFAULT 'text';

CREATE INDEX IF NOT EXISTS idx_documents_content_type
    ON documents (content_type);

COMMIT;
