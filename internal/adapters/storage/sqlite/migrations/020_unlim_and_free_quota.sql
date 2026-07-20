-- 020_unlim_and_free_quota.sql
--
-- Wires the two dormant load-balance flags (prefer_unlim,
-- prefer_free_quota) into the pool router. Prior to this migration both
-- flags were accepted at the API layer but ignored by PickAndLock (see
-- the TODO block in account_store.go).
--
-- Two schema changes:
--
--  1. New table account_unlim_activations — one row per (account,
--     bundle_type) pair, populated by the refresher from
--     GET /workspaces/unlim-activations. bundle_type is the operator-
--     activated bundle (e.g. "nano_banana_2_2k"), job_set_type is the
--     unlim endpoint that bundle unlocks (e.g. "nano_banana_pro_unlimited").
--     PickAndLock joins on job_set_type when the caller sets
--     PickParams.UnlimJobSetType, sorting bundle holders first.
--
--  2. Seven new REAL columns on `accounts` — the per-account free-quota
--     counters returned by GET /user. The refresher writes them on every
--     tick; PickAndLock reads the one column named by
--     PickParams.FreeQuotaField (via a CASE dispatch to keep the column
--     name from becoming user-supplied SQL). Values are floats upstream
--     (e.g. qwen_camera_control_credits: 0.4), so REAL is authoritative.
--
-- Every statement runs inside a single transaction wrapper the migration
-- runner opens (see sqlite.go applyMigrationTx). ALTER TABLE ADD COLUMN
-- is a no-op rewrite in SQLite so this migration is cheap on production
-- pools with tens of accounts.

CREATE TABLE IF NOT EXISTS account_unlim_activations (
  account_id     TEXT NOT NULL,
  bundle_type    TEXT NOT NULL,
  job_set_type   TEXT NOT NULL,
  resolutions    TEXT NOT NULL DEFAULT '',
  expires_at     TEXT,
  activated_at   TEXT NOT NULL,
  synced_at      TEXT NOT NULL,
  PRIMARY KEY (account_id, bundle_type),
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_unlim_by_jst
  ON account_unlim_activations(job_set_type, account_id);

ALTER TABLE accounts ADD COLUMN face_swap_credits            REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN soul_credits                 REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN character_swap_credits       REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN qwen_camera_control_credits  REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN wan2_5_video_credits         REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN text2keyframes_credits       REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN veo3_fast_generations_count  REAL NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO schema_versions (version, applied_at)
  VALUES (20, CURRENT_TIMESTAMP);
