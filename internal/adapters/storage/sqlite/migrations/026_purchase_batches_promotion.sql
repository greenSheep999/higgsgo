-- Migration 026: add promotion_type to purchase_batches, and fix the
-- misclassified unlim_1day row from migration 025.
--
-- BACKGROUND: migration 025 treated "unlim_1day" as a plan_type value
-- and forced its credits_per_account_hundredths to 0. That's wrong on
-- both counts — unlim_1day is a PROMOTION layered on top of a base
-- subscription (starter/pro/plus/ultra), not a plan type of its own.
-- The account MarjamAnbenr1970 was starter (200 credits) with a
-- 1-day-unlim promo bundled in — not "unlim only, no credits".
--
-- DOMAIN MODEL going forward:
--
--   plan_type        — base subscription: starter | pro | plus | ultra
--   promotion_type   — promotional path (three today): none | first_signup
--                      | unlim_1day | standard_credit_boost
--   pricing_class    — outlier tag (orthogonal): normal | activity | bug | promo
--
-- The three promotion_type values the operator identified:
--   first_signup           — free signup bonus, NOT recorded in batches
--                            (excluded by definition — no purchase)
--   unlim_1day             — 1-day unlimited generation quota added on top
--                            of the base plan; MarjamAnbenr1970's case
--   standard_credit_boost  — discounted / boosted standard-credit deal
--                            (some sellers offer bulk credit at reduced
--                            per-credit rates)
--
-- Weighted average impact (contract §10):
-- The pricingfloor.EffectiveUnitCost calculator now filters
-- promotion_type='none' in addition to pricing_class='normal', so
-- promo batches contribute a data point in the UI but don't drag the
-- baseline unit cost. Without this filter, the 1day-unlim row's
-- ($2.86 / 200 credits) would land at 14_300 µ/cr and pull the
-- effective floor 30% lower than reality.

ALTER TABLE purchase_batches
  ADD COLUMN promotion_type TEXT NOT NULL DEFAULT 'none';

-- Fix the seeded unlim_1day row: it's actually a starter purchase with
-- a 1day-unlim promotion bundled in.
UPDATE purchase_batches
SET plan_type = 'starter',
    credits_per_account_hundredths = 20000,
    promotion_type = 'unlim_1day',
    rationale = rationale || ' | migration 026: reclassified from plan_type=unlim_1day to plan_type=starter + promotion_type=unlim_1day',
    updated_at = '2026-07-22T00:00:01Z'
WHERE id = 'pb_tg_cheaplux_marjam_unlim1d';

-- Partial index on the calculator's hot filter combination so the
-- weighted-average read stays fast as the table grows.
DROP INDEX IF EXISTS idx_purchase_batches_active_class;
CREATE INDEX IF NOT EXISTS idx_purchase_batches_calc_eligible
  ON purchase_batches(active, pricing_class, promotion_type)
  WHERE active = 1 AND pricing_class = 'normal' AND promotion_type = 'none';
