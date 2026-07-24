-- 028_pricing_region.sql
--
-- Adds the region axis to official_price_observations and higgs_plan_rates.
--
-- Kuaishou Kling / Bytedance Seedance / Alibaba Wan / MiniMax Hailuo all
-- ship two SKUs — an international one (USD, klingai.com etc.) and a
-- Chinese one (CNY, klingai.kuaishou.com etc.). The pricing matrix has
-- to show them side by side, so every observation row and every plan
-- rate needs to carry which market it belongs to.
--
-- Legal values: 'intl' (default), 'cn'. Additional regions can be added
-- later without a schema change.
--
-- estimated: a raw-pricing/*-cn.md source that documents an inferred
-- value (currency conversion, third-party proxy) sets estimated=1 so the
-- matrix UI can render an "(估算)" badge and floor-suggestion logic can
-- exclude the row from the retail anchor.

ALTER TABLE official_price_observations
  ADD COLUMN region TEXT NOT NULL DEFAULT 'intl';

ALTER TABLE official_price_observations
  ADD COLUMN estimated INTEGER NOT NULL DEFAULT 0;

ALTER TABLE higgs_plan_rates
  ADD COLUMN region TEXT NOT NULL DEFAULT 'intl';

CREATE INDEX IF NOT EXISTS idx_official_price_region
  ON official_price_observations(model_alias, region, observed_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_higgs_plan_rates_region
  ON higgs_plan_rates(region, plan_type, observed_at DESC, id DESC);

INSERT OR IGNORE INTO schema_versions (version, applied_at)
  VALUES (28, CURRENT_TIMESTAMP);
