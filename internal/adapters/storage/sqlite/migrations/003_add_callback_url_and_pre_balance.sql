-- 003_add_callback_url_and_pre_balance.sql
--
-- Two new columns on jobs, both required by the async job lifecycle:
--
--   callback_url   TEXT    caller-supplied webhook URL fired at terminal
--                          state by the pollworker (empty means "no webhook")
--
--   pre_balance_h  INTEGER account subscription_balance snapshot in
--                          credits*100 units captured before upstream job
--                          create. The async metering path reads this at
--                          terminal transition to compute the exact credits
--                          consumed as (pre_balance_h - post.balance);
--                          the sync path already carries this in memory.
--
-- Both default to a benign zero value so pre-existing rows keep working.

ALTER TABLE jobs ADD COLUMN callback_url TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN pre_balance_h INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (3, CURRENT_TIMESTAMP);
