-- 013_failover.sql: automatic account isolation / failover subsystem.
--
-- Introduces two mechanisms driven by the core/failover controller:
--
--   ① consecutive-failure eviction (MVP, on by default). N account-
--      attributable failures in a row disable the account with
--      status='disabled' and a status_reason.
--   ② rate-limit / risk-marker throttling (framework only, off by
--      default). Rolling window of 429-style events cools an account
--      down (status='throttled' with throttled_until = now+cooldown);
--      repeated blacklist episodes within a longer window escalate to
--      disabled.
--
-- Recovery from throttled → active is done by a Recoverer goroutine
-- that flips the row once throttled_until passes; disabled → active
-- is manual only (via POST /admin/accounts/{id}/recover).
--
-- The tables below only carry raw events + per-account overrides. The
-- controller reads global defaults from configs/higgsgo.toml
-- ([failover] section) and merges the row-level override (if any) at
-- decision time.

-- account_failover_events: one row per account-attributable outcome
-- observed by the failover controller. Keeps an audit trail for the
-- admin surface and the sliding-window judge / evict counters.
--
-- kind ∈ {'failure', 'throttle', 'blacklist'}:
--   - 'failure'   : consecutive-fail streak tick (mechanism ①)
--   - 'throttle'  : rate-limit / risk-marker observation (mechanism ②)
--   - 'blacklist' : throttle window judged the account as bot/blocked
--
-- reason is a short opcode ("consec_fail", "risk_marker",
-- "auth_failed", ...) suitable for grouping in the admin UI.
-- http_status is the upstream HTTP code that triggered the event, or
-- 0 for network / transport errors.
CREATE TABLE IF NOT EXISTS account_failover_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id   TEXT NOT NULL,
  kind         TEXT NOT NULL,
  reason       TEXT NOT NULL DEFAULT '',
  http_status  INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_failover_events_account_kind_ts
  ON account_failover_events(account_id, kind, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_failover_events_ts
  ON account_failover_events(created_at DESC);

-- account_failover_overrides: per-account override of the global
-- [failover] tunables. NULL columns fall back to the global config.
-- enabled=0 disables all failover machinery for a single account
-- (useful when an operator wants to keep a shaky test account in the
-- pool without it being auto-disabled).
CREATE TABLE IF NOT EXISTS account_failover_overrides (
  account_id         TEXT PRIMARY KEY,
  enabled            INTEGER,
  fail_limit         INTEGER,
  judge_window_sec   INTEGER,
  judge_count        INTEGER,
  cooldown_sec       INTEGER,
  evict_window_sec   INTEGER,
  evict_count        INTEGER,
  updated_at         TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
);

-- accounts extensions:
--   throttled_until: ISO-8601 timestamp; when non-NULL and in the
--     future, the pool router treats the row as "active-once-recovered"
--     and the Recoverer goroutine flips status back to 'active' after
--     the deadline. NULL means no active cooldown.
--   status_reason: short opcode written by MarkStatus / MarkThrottled
--     so the admin surface can render "why" without joining the events
--     table for every list rendering.
ALTER TABLE accounts ADD COLUMN throttled_until TEXT;
ALTER TABLE accounts ADD COLUMN status_reason   TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_accounts_throttled_until
  ON accounts(throttled_until) WHERE throttled_until IS NOT NULL;

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (13, CURRENT_TIMESTAMP);
