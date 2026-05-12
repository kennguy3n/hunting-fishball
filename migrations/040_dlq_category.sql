-- 040_dlq_category.sql — Round-14 Task 14.
--
-- DLQ message categorisation. The auto-replay worker (Round-12
-- Task 17 — internal/pipeline/dlq_auto_replay.go) currently
-- replays every dead-letter row that has retries remaining. That
-- silently re-runs permanent failures (poison messages, schema
-- violations) on every replay tick, wasting cycles and trapping
-- the row in a retry-loop until MaxAttempts trips.
--
-- The `category` column tags each row at insert time so the
-- auto-replay worker can skip permanents while still draining
-- transients. Operators get a new ?category=transient|permanent
-- filter on the admin endpoint.
--
-- Categories:
--   transient — timeout / 429 / connection refused / 5xx / DNS:
--                worth auto-replaying once the upstream recovers.
--   permanent — parse error / schema violation / poison-message:
--                no point auto-replaying; operator must inspect.
--   unknown   — legacy rows pre-dating Round-14; treated as
--                transient by the worker to preserve existing
--                replay behaviour while operators backfill.

ALTER TABLE dlq_messages
    ADD COLUMN IF NOT EXISTS category VARCHAR(16) NOT NULL DEFAULT 'unknown';

CREATE INDEX IF NOT EXISTS idx_dlq_messages_tenant_category
    ON dlq_messages (tenant_id, category);
