-- 005_apikeys_group_id.sql
--
-- Add a direct 1:1 API-key -> pool group binding column on api_keys.
--
-- The many-to-many apikey_group_bindings table introduced in migration 001
-- stays in place: it is still the source of truth for the (rare) case
-- where one API key needs to be routed to multiple pool groups. But the
-- overwhelming majority of CPA scenarios are 1:1 (a partner's key maps
-- to a single group), and forcing every /v1 generation request to JOIN
-- through the binding table just to discover that one row is wasteful.
--
-- The resolveGroup helper in internal/api/v1 checks api_keys.group_id
-- first (short-circuit) and only falls back to the binding table when
-- this column is empty.
--
-- The column defaults to '' so all existing rows keep behaving exactly
-- as before. The partial index only covers direct-bound rows to avoid
-- indexing every standalone/binding-table-only key with a trivial empty
-- value (same pattern as the cpa_partner_id partial index in 004).

ALTER TABLE api_keys ADD COLUMN group_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_api_keys_group
  ON api_keys(group_id) WHERE group_id != '';

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (5, CURRENT_TIMESTAMP);
