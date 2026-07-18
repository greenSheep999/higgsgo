-- 010_accounts_priority: introduces an operator-managed priority hint on
-- account rows so the pool router can prefer some accounts over others
-- (e.g. paid-tier accounts before free-tier fallbacks). Higher values
-- rank first. Default 0 keeps existing rows neutral until an operator
-- explicitly sets a priority via PATCH /admin/accounts/{id}.
ALTER TABLE accounts ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;

-- The pool router already indexes (status, plan_type, in_flight_jobs,
-- subscription_balance) — priority slots in front of that ordering for
-- pick-and-lock queries.
CREATE INDEX IF NOT EXISTS idx_accounts_priority_pool
  ON accounts(status, priority DESC, in_flight_jobs, subscription_balance DESC);

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (10, CURRENT_TIMESTAMP);
