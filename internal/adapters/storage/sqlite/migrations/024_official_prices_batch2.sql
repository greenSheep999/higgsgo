-- Migration 024: batch-2 import of official upstream API prices.
--
-- Source: docs/audit/THREE-WAY-PRICING-FINAL.md (2026-07-22 audit, 44
-- Higgs models × their upstream API prices, already cross-checked
-- against provider docs). This migration seeds 24 rows spanning 10
-- distinct Higgs aliases:
--
--   seedance-1.5  (480p / 720p / 1080p silent)
--   seedance-2-0  (720p / 1080p / 4k across silent + bundle durations)
--   seedance-2-0-mini
--   minimax-hailuo (Hailuo 02 & 2.3 fast/std × 512p/768p/1080p)
--   kling-2.6      (with/without sound)
--   sora2-video    (720p std/pro + 1080p pro)
--   veo3-1         (720p / 1080p standard)
--   veo-3.1-lite   (720p / 1080p fast)
--   grok-video     (480p / 720p)
--   grok-video-edit
--
-- Batch 1 (migration 023) already covers kling-3 + kling-3-turbo and
-- the four higgs_plan_rates rows; nothing here overlaps. INSERT OR
-- IGNORE keeps re-runs idempotent.
--
-- Rows the audit marked "unknown" or "unverif." are intentionally
-- skipped — see the workflow notes cached in the changelog. Legacy
-- aliases (kling-2.5-turbo, kling-2.1, seedance-pro-1.0, veo-3,
-- veo-3-fast) had no matching JST in verified_models.mjs and are
-- omitted; they can be added via a future migration when those
-- models come online.
--
-- kling3_0 mode fold (contract §4.1): the kling-3 rows in migration
-- 023 use mode='standard' / 'native_audio' for historical reasons;
-- new rows here follow the contract exactly and leave mode='' for
-- kling-family entries whose sub-tier is folded into resolution.

INSERT OR IGNORE INTO official_price_observations
  (id, model_alias, provider, source_url, currency, unit, price_micros, resolution, duration_seconds, mode, audio, dimensions_json, observed_at)
VALUES
  ('op_seedance15_480p_5s_na_off', 'seedance-1.5', 'Bytedance', '', 'USD', 'per_second', 56000, '480p', 5, '', 'off', '{}', '2026-07-22T00:00:00Z'),
  ('op_minimaxhailuo_512p_6s_fast_na', 'minimax-hailuo', 'MiniMax Hailuo', '', 'USD', 'per_second', 80000, '512p', 6, 'fast', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_seedance15_720p_5s_na_off', 'seedance-1.5', 'Bytedance', '', 'USD', 'per_second', 119000, '720p', 5, '', 'off', '{}', '2026-07-22T00:00:00Z'),
  ('op_minimaxhailuo_768p_6s_fast_na', 'minimax-hailuo', 'MiniMax Hailuo', '', 'USD', 'per_second', 186000, '768p', 6, 'fast', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_kling26_na_5s_na_off', 'kling-2.6', 'Kuaishou Kling', '', 'USD', 'per_second', 200000, '', 5, '', 'off', '{}', '2026-07-22T00:00:00Z'),
  ('op_grokvideo_480p_5s_na_na', 'grok-video', 'xAI Grok', '', 'USD', 'per_second', 250000, '480p', 5, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_grokvideo_720p_5s_na_na', 'grok-video', 'xAI Grok', '', 'USD', 'per_second', 350000, '720p', 5, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_minimaxhailuo_768p_6s_na_na', 'minimax-hailuo', 'MiniMax Hailuo', '', 'USD', 'per_second', 266000, '768p', 6, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_seedance15_1080p_5s_na_off', 'seedance-1.5', 'Bytedance', '', 'USD', 'per_second', 269000, '1080p', 5, '', 'off', '{}', '2026-07-22T00:00:00Z'),
  ('op_minimaxhailuo_1080p_6s_fast_na', 'minimax-hailuo', 'MiniMax Hailuo', '', 'USD', 'per_second', 346000, '1080p', 6, 'fast', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_grokvideoedit_720p_5s_na_na', 'grok-video-edit', 'xAI Grok', '', 'USD', 'per_second', 350000, '720p', 5, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_sora2video_720p_4s_na_na', 'sora2-video', 'OpenAI', '', 'USD', 'per_second', 400000, '720p', 4, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_kling26_na_5s_na_on', 'kling-2.6', 'Kuaishou Kling', '', 'USD', 'per_second', 660000, '', 5, '', 'on', '{}', '2026-07-22T00:00:00Z'),
  ('op_minimaxhailuo_1080p_6s_na_na', 'minimax-hailuo', 'MiniMax Hailuo', '', 'USD', 'per_second', 532000, '1080p', 6, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_veo31lite_720p_4s_fast_na', 'veo-3.1-lite', 'Google Vertex', '', 'USD', 'per_second', 400000, '720p', 4, 'fast', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_veo31lite_1080p_4s_fast_na', 'veo-3.1-lite', 'Google Vertex', '', 'USD', 'per_second', 480000, '1080p', 4, 'fast', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_seedance20mini_720p_5s_na_na', 'seedance-2-0-mini', 'Bytedance', '', 'USD', 'per_second', 556000, '720p', 5, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_seedance20_720p_5s_na_na', 'seedance-2-0', 'Bytedance', '', 'USD', 'per_second', 690000, '720p', 5, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_sora2video_720p_4s_pro_na', 'sora2-video', 'OpenAI', '', 'USD', 'per_second', 1200000, '720p', 4, 'pro', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_veo31_720p_4s_standard_na', 'veo3-1', 'Google Vertex', '', 'USD', 'per_second', 1600000, '720p', 4, 'standard', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_veo31_1080p_4s_standard_na', 'veo3-1', 'Google Vertex', '', 'USD', 'per_second', 1600000, '1080p', 4, 'standard', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_seedance20_1080p_5s_na_na', 'seedance-2-0', 'Bytedance', '', 'USD', 'per_second', 1720000, '1080p', 5, '', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_sora2video_1080p_4s_pro_na', 'sora2-video', 'OpenAI', '', 'USD', 'per_second', 2800000, '1080p', 4, 'pro', '', '{}', '2026-07-22T00:00:00Z'),
  ('op_seedance20_4k_5s_na_na', 'seedance-2-0', 'Bytedance', '', 'USD', 'per_second', 3510000, '4k', 5, '', '', '{}', '2026-07-22T00:00:00Z');
