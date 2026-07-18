-- 014_admin_bearer_override: adds a generic key/value table used to
-- persist runtime overrides for configuration values that operators
-- may change from the WebUI (currently only the admin bearer).
--
-- The table is intentionally kept as a plain key/value store rather
-- than adding a bespoke column: future operator-editable settings
-- (e.g. webhook signing key, rate-limit knobs) can land here without
-- another migration.
--
-- The value column holds the raw string exactly as the operator
-- entered it or as the server generated it. For the admin bearer that
-- means the plaintext token — same threat model as configs/*.toml
-- which already stores the plaintext bearer. Access to the SQLite
-- file is equivalent to access to the deploy secret in both cases.
CREATE TABLE IF NOT EXISTS system_settings (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (14, CURRENT_TIMESTAMP);
