-- Migration 025: purchase_batches — one row per real-world procurement
-- event (buying a Higgs account or a top-up), so the "effective unit
-- cost" the pricing floor uses (contract §10) can be computed from
-- actual purchase history instead of a hand-picked config constant.
--
-- Phase-1 kept `pricing.floor_reference_unit_cost_micros` = 27_500 as
-- a conservative single-number default. Phase-2 (this migration + the
-- accompanying admin CRUD + calculator) replaces that with a
-- credit-weighted average across every "normal" batch on record:
--
--   effective_unit_cost = SUM(total_paid_micros)
--                       / SUM(credits_per_account * accounts_count)
--
-- Rows with pricing_class in ('activity','bug','promo') are excluded
-- from the average by default so a one-off deal doesn't drag the
-- floor down. unlim_1day rows carry credits=0 and are naturally
-- excluded (no denominator).
--
-- Design notes:
-- - source_channel and source_seller are SEPARATE columns. TG is a
--   channel; BLACKHATWORLD is a seller inside TG. Future channels:
--   taobao/xianyu/wechat/official/other.
-- - `linked_account_email` is nullable — an account may have been
--   purged (auto-delete after unlim expiry) but the batch itself is
--   an immutable historical fact and must survive.
-- - `active` gates a batch from the weighted average without deleting
--   it, so an operator can "retire" outlier data without losing
--   audit trail.
-- - Credits are stored in "hundredths" (× 100) to stay integer-safe
--   with the rest of the pricing pipeline (higgs_plan_rates uses the
--   same convention).

CREATE TABLE IF NOT EXISTS purchase_batches (
  id                          TEXT PRIMARY KEY,
  purchased_at                TEXT NOT NULL,
  source_channel              TEXT NOT NULL,
  source_seller               TEXT NOT NULL DEFAULT '',
  plan_type                   TEXT NOT NULL,
  accounts_count              INTEGER NOT NULL DEFAULT 1,
  credits_per_account_hundredths INTEGER NOT NULL DEFAULT 0,
  total_paid_micros           INTEGER NOT NULL,
  paid_currency               TEXT NOT NULL DEFAULT 'USD',
  paid_amount_original_micros INTEGER NOT NULL DEFAULT 0,
  exchange_rate_used          REAL NOT NULL DEFAULT 1.0,
  pricing_class               TEXT NOT NULL DEFAULT 'normal',
  active                      INTEGER NOT NULL DEFAULT 1,
  linked_account_email        TEXT NOT NULL DEFAULT '',
  rationale                   TEXT NOT NULL DEFAULT '',
  created_at                  TEXT NOT NULL,
  updated_at                  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_purchase_batches_purchased
  ON purchase_batches(purchased_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_purchase_batches_active_class
  ON purchase_batches(active, pricing_class)
  WHERE active = 1 AND pricing_class = 'normal';

-- Seed: 10 real batches recorded by operator, 2026-07 TG sourcing run.
-- Purchase date column is filled with the recording date (2026-07-22
-- audit snapshot) because the original TG receipts don't carry a
-- per-transaction timestamp — operator can edit later via UI if
-- individual dates surface.
--
-- Notes on individual rows:
-- - Row 7 (MarjamAnbenr1970@hotmail.com) is an unlim_1day top-up: no
--   credits, so credits_per_account_hundredths=0 and it's excluded
--   from the weighted average automatically. The linked account has
--   since been purged from the accounts table — this row still
--   records the $2.86 spend as historical fact.
-- - Row 8 (e7snnrta97@vietnamcashewnuts.store) is plus, not starter.
--   The operator's TG note had the plan wrong; the accounts table
--   confirms plan_type='plus'. Price corrected to $11.20 from an
--   earlier $5.34 typo — $5.34 was implausibly low vs. per-credit
--   parity with starter.
INSERT OR IGNORE INTO purchase_batches
  (id, purchased_at, source_channel, source_seller, plan_type,
   accounts_count, credits_per_account_hundredths, total_paid_micros,
   paid_currency, paid_amount_original_micros, exchange_rate_used,
   pricing_class, active, linked_account_email, rationale,
   created_at, updated_at)
VALUES
  ('pb_tg_blackhat_ujdgvk9wtv',
   '2026-07-22T00:00:00Z', 'tg', 'BLACKHATWORLD', 'starter',
   1, 20000, 5610000, 'USD', 5610000, 1.0,
   'normal', 1, 'ujdgvk9wtv@daivietartex.bond', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_blackhat_8m6dqd3u5t',
   '2026-07-22T00:00:00Z', 'tg', 'BLACKHATWORLD', 'starter',
   1, 20000, 5610000, 'USD', 5610000, 1.0,
   'normal', 1, '8m6dqd3u5t@cc32.headcc.io.vn', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_blackhat_8zucr7kiy8',
   '2026-07-22T00:00:00Z', 'tg', 'BLACKHATWORLD', 'starter',
   1, 20000, 5610000, 'USD', 5610000, 1.0,
   'normal', 1, '8zucr7kiy8@hubcrypto.site', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_cheaplux_6fs1fue2bf',
   '2026-07-22T00:00:00Z', 'tg', 'CheapLuxuryAI', 'starter',
   1, 20000, 5610000, 'USD', 5610000, 1.0,
   'normal', 1, '6fs1fue2bf@cc4.headcc.io.vn', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_cheaplux_hh4p2htnkq',
   '2026-07-22T00:00:00Z', 'tg', 'CheapLuxuryAI', 'starter',
   1, 20000, 5610000, 'USD', 5610000, 1.0,
   'normal', 1, 'hh4p2htnkq@whisperwindwalruswhimsy.site', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_cheaplux_ch2ln3608m',
   '2026-07-22T00:00:00Z', 'tg', 'CheapLuxuryAI', 'starter',
   1, 20000, 5610000, 'USD', 5610000, 1.0,
   'normal', 1, 'ch2ln3608m@sorashift.store', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_cheaplux_marjam_unlim1d',
   '2026-07-22T00:00:00Z', 'tg', 'CheapLuxuryAI', 'unlim_1day',
   1, 0, 2860000, 'USD', 2860000, 1.0,
   'normal', 1, 'MarjamAnbenr1970@hotmail.com',
   'linked account since purged from accounts table (unlim_1day expiry auto-clean); batch retained as historical fact',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_cheaplux_e7snnrta97',
   '2026-07-22T00:00:00Z', 'tg', 'CheapLuxuryAI', 'plus',
   1, 100000, 11200000, 'USD', 11200000, 1.0,
   'normal', 1, 'e7snnrta97@vietnamcashewnuts.store',
   'plan_type corrected from starter to plus per accounts table; paid corrected from earlier $5.34 typo to $11.20',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_cheaplux_von7qxenjs',
   '2026-07-22T00:00:00Z', 'tg', 'CheapLuxuryAI', 'starter',
   1, 20000, 5340000, 'USD', 5340000, 1.0,
   'normal', 1, 'von7qxenjs@pixelpho.space', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z'),
  ('pb_tg_cheaplux_owmr1rhwz1',
   '2026-07-22T00:00:00Z', 'tg', 'CheapLuxuryAI', 'pro',
   1, 60000, 11810000, 'USD', 11810000, 1.0,
   'normal', 1, 'owmr1rhwz1@vietnamcashewnuts.space', '',
   '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z');
