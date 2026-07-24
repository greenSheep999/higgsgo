-- 022_pricing_source.sql
-- Immutable upstream pricing snapshots plus normalized cost rules.

CREATE TABLE IF NOT EXISTS pricing_snapshots (
  id              TEXT PRIMARY KEY,
  source          TEXT NOT NULL,
  source_url      TEXT NOT NULL DEFAULT '',
  payload_json    TEXT NOT NULL,
  payload_sha256  TEXT NOT NULL,
  fetched_at      TEXT NOT NULL,
  effective_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_pricing_snapshots_source_fetched
  ON pricing_snapshots(source, fetched_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS model_cost_rules (
  id                    TEXT PRIMARY KEY,
  snapshot_id           TEXT NOT NULL,
  jst                   TEXT NOT NULL,
  model_alias           TEXT NOT NULL DEFAULT '',
  unit                  TEXT NOT NULL,
  component             TEXT NOT NULL DEFAULT '',
  credits_hundredths    INTEGER NOT NULL,
  original_credits_hundredths INTEGER NOT NULL DEFAULT 0,
  resolution            TEXT NOT NULL DEFAULT '',
  duration_seconds      INTEGER NOT NULL DEFAULT 0,
  mode                  TEXT NOT NULL DEFAULT '',
  audio                 TEXT NOT NULL DEFAULT '',
  dimensions_json       TEXT NOT NULL DEFAULT '{}',
  observed_at           TEXT NOT NULL,
  FOREIGN KEY (snapshot_id) REFERENCES pricing_snapshots(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_model_cost_rules_snapshot_jst
  ON model_cost_rules(snapshot_id, jst);

CREATE TABLE IF NOT EXISTS model_sell_policies (
  model_alias    TEXT PRIMARY KEY,
  currency       TEXT NOT NULL DEFAULT 'USD',
  markup_pct     REAL NOT NULL DEFAULT 1.0,
  minimum_price  REAL NOT NULL DEFAULT 0,
  fixed_price    REAL,
  rounding_unit  REAL NOT NULL DEFAULT 0.001,
  enabled        INTEGER NOT NULL DEFAULT 1,
  updated_at     TEXT NOT NULL
);

INSERT OR IGNORE INTO schema_versions (version, applied_at)
  VALUES (22, CURRENT_TIMESTAMP);
