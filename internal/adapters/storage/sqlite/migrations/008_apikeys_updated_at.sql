-- 008_apikeys_updated_at.sql
--
-- Add an updated_at column to api_keys so the admin write ops
-- (rotate / pause / resume / reset_usage) can record when the row
-- last changed. The Revoke path could stamp this too, but we keep
-- the existing UPDATE unchanged to avoid churn — new operator surfaces
-- are the only writers today.
--
-- Backfill uses created_at so rows written before this migration have
-- a meaningful value instead of NULL. SQLite's ALTER TABLE ADD COLUMN
-- forbids a non-constant default, so the DEFAULT is '' and the
-- backfill runs in the same migration.

ALTER TABLE api_keys ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';

UPDATE api_keys
   SET updated_at = created_at
 WHERE updated_at = '';

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (8, CURRENT_TIMESTAMP);
