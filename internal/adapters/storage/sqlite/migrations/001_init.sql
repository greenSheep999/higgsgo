-- 001_init.sql
-- Initial higgsgo schema. All timestamps stored as ISO-8601 strings for
-- SQLite portability. Balances stored as integers in credits*100 units
-- (higgsfield's internal representation) to avoid floating-point drift.

PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;

-- accounts: the account pool.
CREATE TABLE IF NOT EXISTS accounts (
  id                       TEXT PRIMARY KEY,
  email                    TEXT UNIQUE NOT NULL,
  password_enc             TEXT NOT NULL,
  session_id               TEXT NOT NULL,
  cookies_json             TEXT NOT NULL,
  user_agent               TEXT NOT NULL,
  datadome_client_id       TEXT,
  workspace_id             TEXT,

  plan_type                TEXT NOT NULL,
  has_unlim                INTEGER NOT NULL DEFAULT 0,
  has_flex_unlim           INTEGER NOT NULL DEFAULT 0,
  is_pro_veo3_available    INTEGER NOT NULL DEFAULT 0,
  cohort                   TEXT,

  subscription_balance     INTEGER NOT NULL DEFAULT 0,
  credits_balance          INTEGER NOT NULL DEFAULT 0,
  total_plan_credits       INTEGER NOT NULL DEFAULT 0,
  plan_ends_at             TEXT,

  status                   TEXT NOT NULL DEFAULT 'active',
  in_flight_jobs           INTEGER NOT NULL DEFAULT 0,
  last_balance_at          TEXT,
  last_used_at             TEXT,
  last_failed_at           TEXT,
  fail_streak              INTEGER NOT NULL DEFAULT 0,

  bound_proxy_url          TEXT,

  registered_at            TEXT NOT NULL,
  imported_at              TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_accounts_pool
  ON accounts(status, plan_type, in_flight_jobs, subscription_balance DESC);

-- api_keys: standalone-mode API credentials.
CREATE TABLE IF NOT EXISTS api_keys (
  id             TEXT PRIMARY KEY,
  key_hash       TEXT UNIQUE NOT NULL,
  name           TEXT NOT NULL,
  created_by     TEXT,
  status         TEXT NOT NULL DEFAULT 'active',
  monthly_quota  INTEGER NOT NULL DEFAULT 0,
  monthly_used   INTEGER NOT NULL DEFAULT 0,
  markup_pct     REAL NOT NULL DEFAULT 1.0,
  created_at     TEXT NOT NULL,
  last_used_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);

-- account_groups: pool subdivisions with independent quotas and routing.
CREATE TABLE IF NOT EXISTS account_groups (
  id                          TEXT PRIMARY KEY,
  name                        TEXT UNIQUE NOT NULL,
  description                 TEXT,
  max_concurrent_jobs         INTEGER,
  max_concurrent_per_account  INTEGER,
  monthly_credit_budget       INTEGER,
  monthly_credit_used         INTEGER NOT NULL DEFAULT 0,
  allowed_models_regex        TEXT,
  blocked_models_regex        TEXT,
  route_strategy              TEXT NOT NULL DEFAULT 'round_robin',
  owner_type                  TEXT NOT NULL,
  owner_id                    TEXT,
  status                      TEXT NOT NULL DEFAULT 'active',
  created_at                  TEXT NOT NULL
);

-- account_group_members: many-to-many between accounts and groups.
CREATE TABLE IF NOT EXISTS account_group_members (
  account_id  TEXT NOT NULL,
  group_id    TEXT NOT NULL,
  priority    INTEGER NOT NULL DEFAULT 100,
  added_at    TEXT NOT NULL,
  PRIMARY KEY (account_id, group_id),
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
  FOREIGN KEY (group_id)   REFERENCES account_groups(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_group_members_by_group ON account_group_members(group_id, priority DESC);

-- apikey_group_bindings: which groups an api key may consume.
CREATE TABLE IF NOT EXISTS apikey_group_bindings (
  api_key_id  TEXT NOT NULL,
  group_id    TEXT NOT NULL,
  PRIMARY KEY (api_key_id, group_id),
  FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE,
  FOREIGN KEY (group_id)   REFERENCES account_groups(id) ON DELETE CASCADE
);

-- jobs: every proxied generation.
CREATE TABLE IF NOT EXISTS jobs (
  id                       TEXT PRIMARY KEY,
  api_key_id               TEXT,
  cpa_partner_id           TEXT,
  group_id                 TEXT,
  account_id               TEXT NOT NULL,

  model_alias              TEXT NOT NULL,
  jst                      TEXT NOT NULL,
  endpoint                 TEXT NOT NULL,
  request_body_json        TEXT NOT NULL,
  request_ts               TEXT NOT NULL,

  upstream_job_id          TEXT,
  upstream_cost            INTEGER,
  result_url               TEXT,

  status                   TEXT NOT NULL,
  error_type               TEXT,
  error_detail             TEXT,
  finished_at              TEXT,
  latency_ms               INTEGER,
  poll_count               INTEGER,

  actual_credits_h         INTEGER,
  charged_credits_h        INTEGER,
  refunded                 INTEGER NOT NULL DEFAULT 0,

  FOREIGN KEY (account_id) REFERENCES accounts(id)
);

CREATE INDEX IF NOT EXISTS idx_jobs_account_ts ON jobs(account_id, request_ts);
CREATE INDEX IF NOT EXISTS idx_jobs_api_key_ts ON jobs(api_key_id, request_ts);
CREATE INDEX IF NOT EXISTS idx_jobs_cpa_ts     ON jobs(cpa_partner_id, request_ts);
CREATE INDEX IF NOT EXISTS idx_jobs_status     ON jobs(status);

-- usage_events: per-job accounting rows.
CREATE TABLE IF NOT EXISTS usage_events (
  id                  TEXT PRIMARY KEY,
  ts                  TEXT NOT NULL,
  api_key_id          TEXT,
  cpa_partner_id      TEXT,
  cpa_user_id         TEXT,
  group_id            TEXT NOT NULL,
  account_id          TEXT NOT NULL,
  model_alias         TEXT NOT NULL,
  jst                 TEXT NOT NULL,
  media_type          TEXT NOT NULL,

  upstream_cost       INTEGER,
  actual_credits_h    INTEGER NOT NULL DEFAULT 0,
  charged_credits_h   INTEGER NOT NULL DEFAULT 0,
  markup_pct          REAL NOT NULL DEFAULT 1.0,

  status              TEXT NOT NULL,
  latency_ms          INTEGER,
  poll_count          INTEGER,
  error_type          TEXT,

  higgsgo_job_id      TEXT NOT NULL,
  upstream_job_id     TEXT,
  result_url          TEXT,

  billing_month       TEXT NOT NULL,
  billing_day         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_usage_apikey_month ON usage_events(api_key_id, billing_month);
CREATE INDEX IF NOT EXISTS idx_usage_cpa_month    ON usage_events(cpa_partner_id, billing_month);
CREATE INDEX IF NOT EXISTS idx_usage_group_day    ON usage_events(group_id, billing_day);
CREATE INDEX IF NOT EXISTS idx_usage_account_day  ON usage_events(account_id, billing_day);
CREATE INDEX IF NOT EXISTS idx_usage_model_day    ON usage_events(model_alias, billing_day);

-- usage_daily_agg: pre-aggregated rollup for fast dashboards.
CREATE TABLE IF NOT EXISTS usage_daily_agg (
  day                  TEXT NOT NULL,
  api_key_id           TEXT,
  cpa_partner_id       TEXT,
  group_id             TEXT,
  account_id           TEXT,
  model_alias          TEXT,

  request_count        INTEGER NOT NULL DEFAULT 0,
  completed_count      INTEGER NOT NULL DEFAULT 0,
  failed_count         INTEGER NOT NULL DEFAULT 0,
  refunded_count       INTEGER NOT NULL DEFAULT 0,
  total_credits_h      INTEGER NOT NULL DEFAULT 0,
  charged_credits_h    INTEGER NOT NULL DEFAULT 0,
  total_latency_ms     INTEGER NOT NULL DEFAULT 0,

  updated_at           TEXT NOT NULL,
  PRIMARY KEY (day, api_key_id, cpa_partner_id, group_id, account_id, model_alias)
);

-- registrations: pending / completed account registration attempts.
CREATE TABLE IF NOT EXISTS registrations (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  email          TEXT NOT NULL,
  password       TEXT,
  oauth_source   TEXT,
  refresh_token  TEXT,
  proxy_url      TEXT,
  status         TEXT NOT NULL DEFAULT 'pending',
  attempts       INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT,
  account_id     TEXT,
  created_at     TEXT NOT NULL,
  finished_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_registrations_status ON registrations(status);

-- proxy_pool: the IP proxy pool.
CREATE TABLE IF NOT EXISTS proxy_pool (
  url             TEXT PRIMARY KEY,
  provider        TEXT,
  region          TEXT,
  bound_to        TEXT,
  status          TEXT NOT NULL DEFAULT 'active',
  last_health_at  TEXT,
  last_used_at    TEXT,
  latency_ms      INTEGER
);

CREATE INDEX IF NOT EXISTS idx_proxy_pool_status ON proxy_pool(status, region);

-- model_health: recheck history.
CREATE TABLE IF NOT EXISTS model_health (
  jst            TEXT NOT NULL,
  checked_at     TEXT NOT NULL,
  verdict        TEXT NOT NULL,
  http_status    INTEGER,
  cost           INTEGER,
  poll_time_sec  INTEGER,
  PRIMARY KEY (jst, checked_at)
);

CREATE INDEX IF NOT EXISTS idx_health_recent ON model_health(jst, checked_at DESC);

-- schema_versions: track applied migrations.
CREATE TABLE IF NOT EXISTS schema_versions (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (1, CURRENT_TIMESTAMP);
