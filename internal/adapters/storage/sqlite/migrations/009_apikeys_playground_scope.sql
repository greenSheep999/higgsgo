-- 009_apikeys_playground_scope.sql
--
-- Add a playground_scope column to api_keys so the WebUI /playground
-- surface can gate which models a given key is allowed to invoke
-- interactively. Values are one of:
--
--   * 'none'  (default) — key cannot use /v1/playground/* at all
--   * 'cheap'           — key can test cheap models only
--                         (spec.est_cost_hundredths <= 500, i.e. 5 credits)
--   * 'full'            — key can test any registered model
--
-- The column defaults to 'none' so existing rows keep behaving as if the
-- feature were absent: without an explicit operator opt-in, no key can
-- reach the playground routes. The partial index only covers non-'none'
-- rows since 'none' is by far the most common value and does not need an
-- index for the operator "which keys have playground access" query.

ALTER TABLE api_keys ADD COLUMN playground_scope TEXT NOT NULL DEFAULT 'none';

CREATE INDEX IF NOT EXISTS idx_api_keys_playground_scope
    ON api_keys(playground_scope) WHERE playground_scope != 'none';

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (9, CURRENT_TIMESTAMP);
