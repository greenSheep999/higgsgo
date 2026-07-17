-- 004_apikeys_cpa_partner_id.sql
--
-- Promote the CPA partner id from an encoded created_by prefix
-- ("cpa:" + partner_id) to a first-class column on api_keys. The prefix
-- hack in cpaplugin.register.go is retired in the same change: the
-- handler now writes CPAPartnerID directly and lookups walk the new
-- index instead of a Go-side prefix filter over api_keys.List.
--
-- The column defaults to '' so pre-existing standalone keys keep working
-- unchanged. The partial index only covers CPA-scoped rows to avoid
-- indexing every standalone key with a trivial empty value.
--
-- The final UPDATE backfills any local rows that were written with the
-- old "cpa:" prefix before this migration landed so a dev DB carried
-- over from earlier commits still resolves partner lookups correctly.
-- Production has never carried the old encoding, so this is defensive.

ALTER TABLE api_keys ADD COLUMN cpa_partner_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_api_keys_cpa_partner
  ON api_keys(cpa_partner_id) WHERE cpa_partner_id != '';

UPDATE api_keys
   SET cpa_partner_id = SUBSTR(created_by, 5)
 WHERE created_by LIKE 'cpa:%'
   AND cpa_partner_id = '';

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (4, CURRENT_TIMESTAMP);
