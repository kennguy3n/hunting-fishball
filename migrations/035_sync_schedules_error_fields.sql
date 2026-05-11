-- 035_sync_schedules_error_fields.sql
-- Round-12 follow-up — Devin Review finding on Task 4.
--
-- Round 12 Task 4 added `last_error` (VARCHAR(1024)) and
-- `last_error_at` (TIMESTAMPTZ) fields to the SyncSchedule GORM
-- model and started writing them in scheduler.Tick. The original
-- table from 013_sync_schedules.sql did not have these columns,
-- and there was no migration to add them — production schema
-- could only acquire them via AutoMigrate, which is not part of
-- the deploy path here. Without the columns, the success-path
-- UPDATE in Tick (next_run_at, last_run_at, last_error,
-- last_error_at) would fail wholesale, leaving next_run_at
-- frozen and causing the scheduler to re-fire the same row on
-- every tick.
--
-- This migration backfills the columns so production schema
-- matches the GORM model. Both columns are nullable so the
-- ADD COLUMN is non-blocking on Postgres (no rewrite, no default
-- backfill scan). The default-empty case is represented as
-- NULL rather than '' / epoch; the scheduler success path
-- writes a literal '' and the Go zero time which GORM maps to
-- a non-NULL row, but historic rows stay NULL — both states
-- mean "no recorded error" and the API renders accordingly via
-- the `omitempty` JSON tag.

ALTER TABLE sync_schedules ADD COLUMN IF NOT EXISTS last_error VARCHAR(1024);
ALTER TABLE sync_schedules ADD COLUMN IF NOT EXISTS last_error_at TIMESTAMPTZ;
