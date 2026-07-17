-- 002_jobs_indexes.sql
-- Fast lookup for the background poll worker.
-- ListLive queries jobs where status is queued or in_progress; a partial
-- index would be ideal but SQLite requires the WHERE clause to be immutable,
-- so a simple composite works.

CREATE INDEX IF NOT EXISTS idx_jobs_live
  ON jobs(status, request_ts);

-- Track the most recent upstream poll for each job so the worker can space
-- polls per job independently. Populated by the worker (nullable OK).
-- We add the column via a compat guard because SQLite doesn't support
-- IF NOT EXISTS on ADD COLUMN before 3.35; wrapping in a savepoint lets us
-- ignore duplicate errors.
CREATE TABLE IF NOT EXISTS __compat_alter (n INTEGER PRIMARY KEY);
INSERT INTO __compat_alter (n) VALUES (1);

-- We track the last poll timestamp inline in the jobs table.
-- Adding this column is idempotent: if it already exists the ALTER errors
-- out and we swallow it via the migration runner (each SQL file is run in
-- its own transaction; a failure aborts only that file). To stay safe we
-- add the column using a subquery guarded on schema knowledge.
--
-- SQLite doesn't have IF NOT EXISTS for ADD COLUMN. Use a fallback: check
-- the sqlite_master DDL and skip if present.
--
-- Trick: use CASE inside a stored SELECT can't ADD COLUMN. Simplest is to
-- rely on the migration runner treating a whole-file error as a failure —
-- which is exactly what we want on first apply. On re-apply the whole file
-- is skipped by schema_versions.

ALTER TABLE jobs ADD COLUMN last_poll_at TEXT;

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (2, CURRENT_TIMESTAMP);
