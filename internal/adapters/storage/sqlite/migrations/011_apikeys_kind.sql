-- 011_apikeys_kind: introduces a `kind` classifier on api_keys so the
-- WebUI can distinguish operator "default" keys (broad access, used
-- from the admin console) from downstream "project" keys (shared with
-- partners / teams; tight defaults). Historic rows land as "project"
-- so their behaviour is unchanged.
--
-- Values: default | project. Kept as TEXT for forward compatibility.
ALTER TABLE api_keys ADD COLUMN kind TEXT NOT NULL DEFAULT 'project';

CREATE INDEX IF NOT EXISTS idx_api_keys_kind ON api_keys(kind);

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (11, CURRENT_TIMESTAMP);
