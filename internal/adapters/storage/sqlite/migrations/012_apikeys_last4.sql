-- 012_apikeys_last4: stores the last 4 characters of the plaintext key
-- body so the admin list can render a partial preview (`sk-hg-•••xyz1`)
-- like OpenAI / Anthropic do. The full plaintext remains
-- unrecoverable; only these 4 chars are stored, which is not enough
-- to reconstruct anything but plenty to visually distinguish rows.
--
-- Existing rows land with an empty string; those keys will simply
-- render fully-masked (sk-hg-••••••••) until they are rotated and
-- the new last-4 is written back.
ALTER TABLE api_keys ADD COLUMN key_last4 TEXT NOT NULL DEFAULT '';

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (12, CURRENT_TIMESTAMP);
