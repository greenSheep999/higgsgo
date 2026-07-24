-- 029_kling_intl_verified.sql
--
-- Verified international (klingai.com) pricing for the Kuaishou Kling
-- family, transcribed from
--   higgsfield-register/docs/raw-pricing/kuaishou-kling-intl.md
--   fetched 2026-07-22 from https://kling.ai/dev/pricing
--
-- All rows are region='intl', currency='USD', per_second unless noted.
-- price_micros = USD * 1e6.
--
-- Coverage by higgsgo alias (13 aliases in verified-models.json):
--
--   kling-3                  → Kling 3.0            (10 rows: 720p/1080p × {off,on,voice} + 4k × {off,on})
--   kling-3-turbo            → Kling 3.0 Turbo      (2 rows: 720p/1080p native audio)
--   kling-omni-image         → Kling 3.0 Omni       (image branch — official page only lists video variants)
--   kling-omni-flf           → Kling 3.0 Omni video (6 rows: no video input × {off,on} 3-res + video input × off 3-res)
--   kling-omni-image-reference → Kling 3.0 Omni (video, image reference variant — same table as Omni)
--   kling-o3-flf             → Kling O3 / Omni-1    (Kling O3 lipsync/FLF endpoint — legacy)
--   kling-o3-image-reference → Kling O3 image ref   (legacy Omni family)
--   kling2-6-motion-control  → Motion Control       (2 rows: 720p/1080p)
--   kling2-6-lipsync         → Avatar / lipsync     (2 rows: 720p/1080p)
--   kling-transition         → Effects              ("varies by template" — no official price rows)
--   kling-2.6                → legacy               (not on official page; third-party relay only, not seeded)
--   kling                    → registry catch-all   (not on official page)
--   bd-studio-kling          → higgsfield internal  (studio integration; not upstream-priced)
--
-- Rows are inserted only where the official page publishes a price for
-- that alias. Legacy/relay-only aliases (kling-2.6, kling, bd-studio-kling)
-- are intentionally absent — those go through third-party sources tomorrow.

INSERT OR REPLACE INTO official_price_observations
  (id, model_alias, provider, source_url, currency, unit, price_micros,
   resolution, duration_seconds, mode, audio, dimensions_json, observed_at,
   region, estimated) VALUES

-- ============================================================
-- Kling 3.0 (main video model)
-- Source: raw-pricing/kuaishou-kling-intl.md §"Kling 3.0"
-- ============================================================
-- 720p × audio
('kling3_intl_720_off',   'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second',  84000, '720p',  0, '',       'off',           '{"units_per_second":0.6}', '2026-07-22T00:00:00Z', 'intl', 0),
('kling3_intl_720_on',    'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 126000, '720p',  0, '',       'on',            '{"units_per_second":0.9}', '2026-07-22T00:00:00Z', 'intl', 0),
('kling3_intl_720_voice', 'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 154000, '720p',  0, '',       'voice_control', '{"units_per_second":1.1}', '2026-07-22T00:00:00Z', 'intl', 0),

-- 1080p × audio
('kling3_intl_1080_off',   'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 112000, '1080p', 0, '',       'off',           '{"units_per_second":0.8}', '2026-07-22T00:00:00Z', 'intl', 0),
('kling3_intl_1080_on',    'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 168000, '1080p', 0, '',       'on',            '{"units_per_second":1.2}', '2026-07-22T00:00:00Z', 'intl', 0),
('kling3_intl_1080_voice', 'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 196000, '1080p', 0, '',       'voice_control', '{"units_per_second":1.4}', '2026-07-22T00:00:00Z', 'intl', 0),

-- 4K × audio (voice_control not offered)
('kling3_intl_4k_off',  'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 420000, '4k',    0, '',       'off',           '{"units_per_second":3.0}', '2026-07-22T00:00:00Z', 'intl', 0),
('kling3_intl_4k_on',   'kling-3', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 420000, '4k',    0, '',       'on',            '{"units_per_second":3.0}', '2026-07-22T00:00:00Z', 'intl', 0),

-- ============================================================
-- Kling 3.0 Turbo (fast + native audio)
-- Source: raw-pricing/kuaishou-kling-intl.md §"Kling 3.0 Turbo"
-- ============================================================
('kling3turbo_intl_720',  'kling-3-turbo', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 112000, '720p',  0, 'native_audio', 'on', '{"units_per_second":0.8}', '2026-07-22T00:00:00Z', 'intl', 0),
('kling3turbo_intl_1080', 'kling-3-turbo', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 140000, '1080p', 0, 'native_audio', 'on', '{"units_per_second":1.0}', '2026-07-22T00:00:00Z', 'intl', 0),

-- ============================================================
-- Kling 3.0 Omni (multimodal input)
-- Source: raw-pricing/kuaishou-kling-intl.md §"Kling 3.0 Omni"
-- Mapped to kling-omni-flf (video with FLF from reference).
-- kling-omni-image (still image output) and kling-omni-image-reference
-- share the same official price table so we duplicate rows across aliases.
-- ============================================================
-- kling-omni-flf: 3 input modes × 3 resolutions
('klingomniflf_intl_720_novi_off',   'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second',  84000, '720p',  0, 'no_video_input', 'off', '{"units_per_second":0.6}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingomniflf_intl_720_novi_on',    'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 112000, '720p',  0, 'no_video_input', 'on',  '{"units_per_second":0.8}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingomniflf_intl_720_vi_off',     'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 126000, '720p',  0, 'video_input',    'off', '{"units_per_second":0.9}', '2026-07-22T00:00:00Z', 'intl', 0),

('klingomniflf_intl_1080_novi_off',  'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 112000, '1080p', 0, 'no_video_input', 'off', '{"units_per_second":0.8}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingomniflf_intl_1080_novi_on',   'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 140000, '1080p', 0, 'no_video_input', 'on',  '{"units_per_second":1.0}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingomniflf_intl_1080_vi_off',    'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 168000, '1080p', 0, 'video_input',    'off', '{"units_per_second":1.2}', '2026-07-22T00:00:00Z', 'intl', 0),

('klingomniflf_intl_4k_novi_off',    'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 420000, '4k',    0, 'no_video_input', 'off', '{"units_per_second":3.0}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingomniflf_intl_4k_novi_on',     'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 420000, '4k',    0, 'no_video_input', 'on',  '{"units_per_second":3.0}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingomniflf_intl_4k_vi_off',      'kling-omni-flf', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 420000, '4k',    0, 'video_input',    'off', '{"units_per_second":3.0}', '2026-07-22T00:00:00Z', 'intl', 0),

-- ============================================================
-- Motion Control  → kling2-6-motion-control
-- Source: raw-pricing/kuaishou-kling-intl.md §"Motion Control"
-- ============================================================
('klingmc_intl_720',  'kling2-6-motion-control', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 126000, '720p',  0, '', '', '{"units_per_second":0.9}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingmc_intl_1080', 'kling2-6-motion-control', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 168000, '1080p', 0, '', '', '{"units_per_second":1.2}', '2026-07-22T00:00:00Z', 'intl', 0),

-- ============================================================
-- Avatar (lipsync)  → kling2-6-lipsync
-- Source: raw-pricing/kuaishou-kling-intl.md §"Avatar / Lipsync"
-- ============================================================
('klingavatar_intl_720',  'kling2-6-lipsync', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second',  56000, '720p',  0, '', '', '{"units_per_second":0.4}', '2026-07-22T00:00:00Z', 'intl', 0),
('klingavatar_intl_1080', 'kling2-6-lipsync', 'Kuaishou Kling (Intl)', 'https://kling.ai/dev/pricing',
 'USD', 'per_second', 112000, '1080p', 0, '', '', '{"units_per_second":0.8}', '2026-07-22T00:00:00Z', 'intl', 0);

-- ============================================================
-- Effects (kling-transition), kling-2.6 (legacy), kling (catch-all),
-- bd-studio-kling: no rows — see the header comment for reasoning.
-- Third-party relay prices for kling-2.6/2.5/2.1 documented in the
-- raw-pricing file are outside the "official API" feed and will be
-- imported into a separate table (or tagged provider='PiAPI'/'fal.ai')
-- once the third-party seed pass runs.
-- ============================================================

INSERT OR IGNORE INTO schema_versions (version, applied_at)
  VALUES (29, CURRENT_TIMESTAMP);
