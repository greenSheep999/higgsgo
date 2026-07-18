-- 016_account_metadata.sql: adds operator-managed metadata columns to accounts.
-- max_concurrent allows per-account override of the upstream concurrency cap (6).
-- note is a free-form operator memo.
-- source records how the account entered the pool.
ALTER TABLE accounts ADD COLUMN max_concurrent INTEGER NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN note TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN source TEXT NOT NULL DEFAULT '';

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (16, CURRENT_TIMESTAMP);
