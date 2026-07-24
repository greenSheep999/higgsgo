-- 023_pricing_matrix.sql
-- Internal comparison sources and final sell decisions. Money is USD micros.

CREATE TABLE IF NOT EXISTS higgs_plan_rates (
  id                TEXT PRIMARY KEY,
  plan_type         TEXT NOT NULL,
  plan_name         TEXT NOT NULL,
  billing_period    TEXT NOT NULL DEFAULT 'monthly',
  currency          TEXT NOT NULL DEFAULT 'USD',
  amount_micros     INTEGER NOT NULL,
  credits           INTEGER NOT NULL,
  unit_cost_micros  INTEGER NOT NULL,
  source_url        TEXT NOT NULL DEFAULT '',
  observed_at       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_higgs_plan_rates_current
  ON higgs_plan_rates(plan_type, billing_period, observed_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS official_price_observations (
  id                TEXT PRIMARY KEY,
  model_alias       TEXT NOT NULL,
  provider          TEXT NOT NULL,
  source_url        TEXT NOT NULL DEFAULT '',
  currency          TEXT NOT NULL DEFAULT 'USD',
  unit              TEXT NOT NULL,
  price_micros      INTEGER NOT NULL,
  resolution        TEXT NOT NULL DEFAULT '',
  duration_seconds  INTEGER NOT NULL DEFAULT 0,
  mode              TEXT NOT NULL DEFAULT '',
  audio             TEXT NOT NULL DEFAULT '',
  dimensions_json   TEXT NOT NULL DEFAULT '{}',
  observed_at       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_official_price_model_current
  ON official_price_observations(model_alias, observed_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS model_price_decisions (
  id                TEXT PRIMARY KEY,
  model_alias       TEXT NOT NULL,
  currency          TEXT NOT NULL DEFAULT 'USD',
  unit              TEXT NOT NULL,
  price_micros      INTEGER NOT NULL,
  resolution        TEXT NOT NULL DEFAULT '',
  duration_seconds  INTEGER NOT NULL DEFAULT 0,
  mode              TEXT NOT NULL DEFAULT '',
  audio             TEXT NOT NULL DEFAULT '',
  dimensions_json   TEXT NOT NULL DEFAULT '{}',
  rationale         TEXT NOT NULL DEFAULT '',
  decided_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_model_price_decisions_current
  ON model_price_decisions(model_alias, decided_at DESC, id DESC);

-- Initial plan baselines from the operator-maintained 2026-07-22 snapshot.
INSERT OR IGNORE INTO higgs_plan_rates
  (id, plan_type, plan_name, billing_period, currency, amount_micros, credits, unit_cost_micros, source_url, observed_at)
VALUES
  ('plan_starter_20260722', 'starter', 'Starter', 'monthly', 'USD', 15000000, 200, 75000, 'https://higgsfield.ai/pricing', '2026-07-22T00:00:00Z'),
  ('plan_pro_20260722',     'pro',     'Pro',     'monthly', 'USD', 29000000, 600, 48333, 'https://higgsfield.ai/pricing', '2026-07-22T00:00:00Z'),
  ('plan_plus_20260722',    'plus',    'Plus',    'monthly', 'USD', 49000000, 1000, 49000, 'https://higgsfield.ai/pricing', '2026-07-22T00:00:00Z'),
  ('plan_ultra_20260722',   'ultra',   'Ultra',   'monthly', 'USD', 129000000, 3000, 43000, 'https://higgsfield.ai/pricing', '2026-07-22T00:00:00Z');

-- First structured import from docs/raw-pricing/kuaishou-kling-intl.md.
INSERT OR IGNORE INTO official_price_observations
  (id, model_alias, provider, source_url, currency, unit, price_micros, resolution, mode, audio, dimensions_json, observed_at)
VALUES
  ('kling3_720_off',   'kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second',  84000, '720p',  'standard', 'off',           '{}', '2026-07-22T00:00:00Z'),
  ('kling3_1080_off',  'kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 112000, '1080p', 'standard', 'off',           '{}', '2026-07-22T00:00:00Z'),
  ('kling3_4k_off',    'kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 420000, '4k',    'standard', 'off',           '{}', '2026-07-22T00:00:00Z'),
  ('kling3_720_on',    'kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 126000, '720p',  'standard', 'on',            '{}', '2026-07-22T00:00:00Z'),
  ('kling3_1080_on',   'kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 168000, '1080p', 'standard', 'on',            '{}', '2026-07-22T00:00:00Z'),
  ('kling3_4k_on',     'kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 420000, '4k',    'standard', 'on',            '{}', '2026-07-22T00:00:00Z'),
  ('kling3_720_voice', 'kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 154000, '720p',  'standard', 'voice_control', '{}', '2026-07-22T00:00:00Z'),
  ('kling3_1080_voice','kling-3', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 196000, '1080p', 'standard', 'voice_control', '{}', '2026-07-22T00:00:00Z'),
  ('kling3t_720_on',   'kling-3-turbo', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 112000, '720p',  'native_audio', 'on', '{}', '2026-07-22T00:00:00Z'),
  ('kling3t_1080_on',  'kling-3-turbo', 'Kuaishou Kling', 'https://kling.ai/dev/pricing', 'USD', 'per_second', 140000, '1080p', 'native_audio', 'on', '{}', '2026-07-22T00:00:00Z');
