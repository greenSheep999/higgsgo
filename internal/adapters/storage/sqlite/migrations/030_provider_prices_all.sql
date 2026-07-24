-- Migration 030: seed official_price_observations from raw-pricing/* (all providers).
-- Snapshot date: 2026-07-24. Source of truth: higgsfield-register/docs/raw-pricing/
--
-- Providers covered:
--   Kuaishou Kling  (CN estimated from CNY at 7.15)
--   Bytedance Seedance / Seedream (CN + INTL, from Volcengine + BytePlus docs)
--   Alibaba Wan     (CN + INTL, from Bailian + ModelStudio docs)
--   MiniMax Hailuo  (CN + INTL, per-request Hailuo 2.3 + fast draw mode)
--   Google Veo      (INTL, Veo 3.1 std/lite/speak + Gemini Omni)
--   Google Gemini   (INTL, Nano Banana image family + Imagen)
--   OpenAI Sora     (INTL, Sora 2 std/pro tiers)
--   OpenAI Image    (INTL, gpt-image-1 std/pro at 1k/1.5k)
--   BFL FLUX        (INTL, FLUX.2 klein-4b/klein-9b/pro/flex/max + Kontext)
--   xAI Grok        (INTL, Grok Image + Video v1.0/v1.5)
--   Recraft         (INTL, v4 std/pro)
--   Sync Labs       (INTL, lipsync-v1.9/lipsync-2/sync-3)
--
-- estimated=true rows (typically CN currency-derived) are excluded from
-- /api/pricing wire by HandleDownstreamPricing but visible in admin UI.
--
-- IMPORTANT: The 023 initial migration seeded some fixture Kling data; 029 replaced
-- it with real intl-only Kling scrapes. This migration extends coverage to all
-- providers we currently serve, using DELETE+INSERT so re-running is idempotent.

DELETE FROM official_price_observations WHERE observed_at='2026-07-24T00:00:00Z';

INSERT INTO official_price_observations
  (id, model_alias, provider, unit, price_micros, resolution, audio, mode,
   duration_seconds, currency, source_url, region, estimated, observed_at)
VALUES
  ('obs_96256b08d991bff6', 'flux-2', 'Black Forest Labs FLUX', 'per_request', 50000, '', '', 'flex', 0, 'USD', 'https://bfl.ai/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_a78bad8f647f4b24', 'flux-2', 'Black Forest Labs FLUX', 'per_request', 14000, '', '', 'klein-4b', 0, 'USD', 'https://bfl.ai/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_e7c9971ccea44b29', 'flux-2', 'Black Forest Labs FLUX', 'per_request', 15000, '', '', 'klein-9b', 0, 'USD', 'https://bfl.ai/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_b55732fb6adbf8f1', 'flux-2', 'Black Forest Labs FLUX', 'per_request', 70000, '', '', 'max', 0, 'USD', 'https://bfl.ai/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_3bc522532437b986', 'flux-2', 'Black Forest Labs FLUX', 'per_request', 30000, '', '', 'pro', 0, 'USD', 'https://bfl.ai/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_6b68fb573e36b62a', 'flux-kontext', 'Black Forest Labs FLUX', 'per_request', 80000, '', '', 'pro', 0, 'USD', 'https://bfl.ai/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_8dcca4ba62606e6d', 'flux-kontext', 'Black Forest Labs FLUX', 'per_request', 40000, '', '', 'std', 0, 'USD', 'https://bfl.ai/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_85ca0136c7588a7d', 'gpt-image', 'OpenAI Image', 'per_request', 250000, '1.5k', '', 'pro', 0, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_27ed7cc82e0d4093', 'gpt-image', 'OpenAI Image', 'per_request', 63000, '1.5k', '', 'std', 0, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_cc5c35583b740cf3', 'gpt-image', 'OpenAI Image', 'per_request', 167000, '1k', '', 'pro', 0, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_782a8a9638760735', 'gpt-image', 'OpenAI Image', 'per_request', 42000, '1k', '', 'std', 0, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_bae4bda509887a19', 'grok-image', 'xAI Grok', 'per_request', 50000, '1k', '', 'pro', 0, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_d378665a154069d6', 'grok-image', 'xAI Grok', 'per_request', 20000, '1k', '', 'std', 0, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_3d83378f6b882f97', 'grok-image', 'xAI Grok', 'per_request', 70000, '2k', '', 'pro', 0, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_477482717635b61d', 'grok-image', 'xAI Grok', 'per_request', 20000, '2k', '', 'std', 0, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_a7c6d133d212450c', 'grok-video', 'xAI Grok', 'per_second', 50000, '480p', 'on', 'std', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_42a137b2ee65bec4', 'grok-video', 'xAI Grok', 'per_second', 70000, '720p', 'on', 'std', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_3c5d26d06de56e2e', 'grok-video-edit', 'xAI Grok', 'per_second', 250000, '1080p', '', 'pro', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_dfc09151f6ed9389', 'grok-video-edit', 'xAI Grok', 'per_second', 80000, '480p', '', 'pro', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_d92ed2891231102c', 'grok-video-edit', 'xAI Grok', 'per_second', 140000, '720p', '', 'pro', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_e5a3d718b5dfb8fe', 'grok-video-v15', 'xAI Grok', 'per_second', 250000, '1080p', '', 'pro', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_1dce01a707352154', 'grok-video-v15', 'xAI Grok', 'per_second', 80000, '480p', '', 'pro', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_025f5ff1e3b840eb', 'grok-video-v15', 'xAI Grok', 'per_second', 140000, '720p', '', 'pro', 5, 'USD', 'https://docs.x.ai/docs/models', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_c226081bde4ca147', 'imagegen-2', 'Google Gemini', 'per_request', 20000, '', '', 'fast', 0, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_ea4d64a1c819d65c', 'imagegen-2', 'Google Gemini', 'per_request', 60000, '', '', 'pro', 0, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_dee4c692c7c13533', 'imagegen-2', 'Google Gemini', 'per_request', 40000, '', '', 'std', 0, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_99f807347e6b188d', 'kling-3', 'Kuaishou Kling (CN)', 'per_second', 111900, '1080p', 'off', '', 5, 'USD', 'https://klingai.kuaishou.com/dev/pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_acfd38ddbd43c3a4', 'kling-3', 'Kuaishou Kling (CN)', 'per_second', 167800, '1080p', 'on', '', 5, 'USD', 'https://klingai.kuaishou.com/dev/pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_2d6876d0a87dccc9', 'kling-3', 'Kuaishou Kling (CN)', 'per_second', 83900, '720p', 'off', '', 5, 'USD', 'https://klingai.kuaishou.com/dev/pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_97d830d27fd41bbf', 'kling-3', 'Kuaishou Kling (CN)', 'per_second', 125900, '720p', 'on', '', 5, 'USD', 'https://klingai.kuaishou.com/dev/pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_d8f03b470442c0ec', 'minimax-hailuo', 'MiniMax Hailuo (CN)', 'per_request', 490000, '1080p', '', '', 6, 'USD', 'https://platform.minimaxi.com', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_93f68dad66939920', 'minimax-hailuo', 'MiniMax Hailuo (CN)', 'per_request', 280000, '768p', '', '', 6, 'USD', 'https://platform.minimaxi.com', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_94c7ae6807e0f521', 'minimax-hailuo', 'MiniMax Hailuo (CN)', 'per_request', 560000, '768p', '', '', 10, 'USD', 'https://platform.minimaxi.com', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_ad9980159beedaf4', 'minimax-hailuo', 'MiniMax Hailuo (INTL)', 'per_request', 490000, '1080p', '', '', 6, 'USD', 'https://platform.minimax.io/docs/guides/pricing-video', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_5fcf06c83c5506e6', 'minimax-hailuo', 'MiniMax Hailuo (INTL)', 'per_request', 280000, '768p', '', '', 6, 'USD', 'https://platform.minimax.io/docs/guides/pricing-video', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_5fdf99b48c4b8011', 'minimax-hailuo', 'MiniMax Hailuo (INTL)', 'per_request', 560000, '768p', '', '', 10, 'USD', 'https://platform.minimax.io/docs/guides/pricing-video', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_820aef7fa638c3ab', 'minimax-hailuo-draw', 'MiniMax Hailuo (CN)', 'per_request', 330000, '1080p', '', 'fast', 6, 'USD', 'https://platform.minimaxi.com', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_31ed2e9a5b47178a', 'minimax-hailuo-draw', 'MiniMax Hailuo (CN)', 'per_request', 190000, '768p', '', 'fast', 6, 'USD', 'https://platform.minimaxi.com', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_2831fd1a81235711', 'minimax-hailuo-draw', 'MiniMax Hailuo (CN)', 'per_request', 320000, '768p', '', 'fast', 10, 'USD', 'https://platform.minimaxi.com', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_16f02a9e5a85b38e', 'minimax-hailuo-draw', 'MiniMax Hailuo (INTL)', 'per_request', 330000, '1080p', '', 'fast', 6, 'USD', 'https://platform.minimax.io/docs/guides/pricing-video', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_a28ac1d085678947', 'minimax-hailuo-draw', 'MiniMax Hailuo (INTL)', 'per_request', 190000, '768p', '', 'fast', 6, 'USD', 'https://platform.minimax.io/docs/guides/pricing-video', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_14a5cc534c53b743', 'minimax-hailuo-draw', 'MiniMax Hailuo (INTL)', 'per_request', 320000, '768p', '', 'fast', 10, 'USD', 'https://platform.minimax.io/docs/guides/pricing-video', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_d0292b5ddd542b2f', 'nano-banana', 'Google Gemini', 'per_request', 39000, '1k', '', '', 0, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_6aa61d0ceed051bf', 'nano-banana-2', 'Google Gemini', 'per_request', 134000, '2k', '', 'pro', 0, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_e8005d4b16960bdd', 'nano-banana-2', 'Google Gemini', 'per_request', 240000, '4k', '', 'pro', 0, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_cb1383b1fc656640', 'nano-banana-2-lite', 'Google Gemini', 'per_request', 67000, '1k', '', '', 0, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_71742be622430f85', 'recraft-v4', 'Recraft', 'per_request', 40000, '', '', '', 0, 'USD', 'https://recraft.ai/docs/api-reference/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_54c91614b026ba15', 'recraft-v4', 'Recraft', 'per_request', 250000, '', '', 'pro', 0, 'USD', 'https://recraft.ai/docs/api-reference/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_43210169ee7edf1a', 'seedance-1.5', 'Bytedance Volcengine Seedance (CN)', 'per_second', 54300, '1080p', 'off', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_338000c9be62562f', 'seedance-1.5', 'Bytedance Volcengine Seedance (CN)', 'per_second', 108800, '1080p', 'on', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_18af39b60937e20a', 'seedance-1.5', 'Bytedance Volcengine Seedance (CN)', 'per_second', 11200, '480p', 'off', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_ebdfad99044a5ce1', 'seedance-1.5', 'Bytedance Volcengine Seedance (CN)', 'per_second', 22400, '480p', 'on', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_b0079bfb8bdd6f57', 'seedance-1.5', 'Bytedance Volcengine Seedance (CN)', 'per_second', 24100, '720p', 'off', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_309de8f1acd40d99', 'seedance-1.5', 'Bytedance Volcengine Seedance (CN)', 'per_second', 48400, '720p', 'on', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_d6a7daf7b761a2a4', 'seedance-2-0', 'Bytedance Volcengine Seedance (CN)', 'per_second', 346900, '1080p', '', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_d8329288d8b2c30e', 'seedance-2-0', 'Bytedance Volcengine Seedance (CN)', 'per_second', 64300, '480p', '', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_c38d194a830fe135', 'seedance-2-0', 'Bytedance Volcengine Seedance (CN)', 'per_second', 706300, '4k', '', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_99c559e904dee388', 'seedance-2-0', 'Bytedance Volcengine Seedance (CN)', 'per_second', 138500, '720p', '', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_a040cc84f7fe42e0', 'seedance-2-0', 'BytePlus Seedance (INTL)', 'per_second', 344000, '1080p', '', '', 5, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_e5424aa958e128ac', 'seedance-2-0', 'BytePlus Seedance (INTL)', 'per_second', 64000, '480p', '', '', 5, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_f40a06b90a094dec', 'seedance-2-0', 'BytePlus Seedance (INTL)', 'per_second', 701000, '4k', '', '', 5, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_36c928fcdec228df', 'seedance-2-0', 'BytePlus Seedance (INTL)', 'per_second', 138000, '720p', '', '', 5, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_81d9810f3c11d819', 'seedance-2-0-mini', 'Bytedance Volcengine Seedance (CN)', 'per_second', 32200, '480p', '', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_fe4b8a601e8dec24', 'seedance-2-0-mini', 'Bytedance Volcengine Seedance (CN)', 'per_second', 69900, '720p', '', '', 5, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_552d8f2f7c90f355', 'seedance-2-0-mini', 'BytePlus Seedance (INTL)', 'per_second', 32000, '480p', '', '', 5, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_c089afd64360899e', 'seedance-2-0-mini', 'BytePlus Seedance (INTL)', 'per_second', 69000, '720p', '', '', 5, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_19ccfebdf972b23b', 'seedream-5-lite', 'Bytedance Volcengine Seedream (CN)', 'per_request', 30800, '', '', '', 0, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_0507b56901926caf', 'seedream-5-lite', 'BytePlus Seedream (INTL)', 'per_request', 31000, '', '', '', 0, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_258de19aa9a7ceb5', 'seedream-v4-5', 'Bytedance Volcengine Seedream (CN)', 'per_request', 35000, '', '', '', 0, 'USD', 'https://docs.volcengine.com/docs/82379/1544106', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_32bcd1e99fc89ce5', 'seedream-v4-5', 'BytePlus Seedream (INTL)', 'per_request', 35000, '', '', '', 0, 'USD', 'https://docs.byteplus.com/en/docs/ModelArk/1544106', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_3235c144bba3b10e', 'sora2-video', 'OpenAI Sora', 'per_second', 500000, '1024p', 'on', 'pro', 10, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_d3e57270cb851216', 'sora2-video', 'OpenAI Sora', 'per_second', 700000, '1080p', 'on', 'pro', 10, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_fdc087ec6ff39a27', 'sora2-video', 'OpenAI Sora', 'per_second', 300000, '720p', 'on', 'pro', 10, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_6a8fdc4765f529eb', 'sora2-video', 'OpenAI Sora', 'per_second', 100000, '720p', 'on', 'std', 8, 'USD', 'https://developers.openai.com/api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_5e6e30f69b326ad8', 'sync-so', 'Sync Labs', 'per_second', 40000, '', '', 'lipsync-2', 0, 'USD', 'https://sync.so/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_3622a558080e984e', 'sync-so', 'Sync Labs', 'per_second', 20000, '', '', 'lipsync-v1.9', 0, 'USD', 'https://sync.so/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_6a1f2fa59838941b', 'sync-so', 'Sync Labs', 'per_second', 133000, '', '', 'sync-3', 0, 'USD', 'https://sync.so/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_ade1185f2f10cd1d', 'veo-3.1-lite', 'Google Veo', 'per_second', 80000, '1080p', '', '', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_0f569c75b601d7fc', 'veo-3.1-lite', 'Google Veo', 'per_second', 50000, '720p', '', '', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_662f3e8669e8d8ed', 'veo-gemini-omni', 'Google Veo', 'per_second', 400000, '1080p', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_a254155950200f19', 'veo-gemini-omni', 'Google Veo', 'per_second', 400000, '720p', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_e8a76bf51ac589e7', 'veo3-1', 'Google Veo', 'per_second', 400000, '1080p', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_b3175c95a03254b5', 'veo3-1', 'Google Veo', 'per_second', 600000, '4k', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_4df72d19001a6f69', 'veo3-1', 'Google Veo', 'per_second', 400000, '720p', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_e7af0271dff2b089', 'veo3-1-speak', 'Google Veo', 'per_second', 400000, '1080p', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_55b10c26de2982cb', 'veo3-1-speak', 'Google Veo', 'per_second', 600000, '4k', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_0fbd940d1653b66b', 'veo3-1-speak', 'Google Veo', 'per_second', 400000, '720p', 'on', 'std', 8, 'USD', 'https://ai.google.dev/gemini-api/docs/pricing', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_9f5368019bcf1b62', 'wan2-2-animate', 'Alibaba Bailian Wan (CN)', 'per_second', 83900, '', '', 'pro', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_165bf1e715d2764b', 'wan2-2-animate', 'Alibaba Bailian Wan (CN)', 'per_second', 55900, '', '', 'std', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_7f3350954c466fdc', 'wan2-2-image', 'Alibaba Bailian Wan (CN)', 'per_request', 28000, '', '', '', 0, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_1778b12c25c7aefa', 'wan2-2-video', 'Alibaba Bailian Wan (CN)', 'per_second', 97900, '1080p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_060487b2083d8552', 'wan2-2-video', 'Alibaba Bailian Wan (CN)', 'per_second', 19600, '480p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_8ab290c843b3c239', 'wan2-2-video', 'Alibaba ModelStudio Wan (INTL)', 'per_second', 19000, '480p', '', '', 5, 'USD', 'https://modelstudio.console.alibabacloud.com', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_237b0ed6038f883c', 'wan2-5-video', 'Alibaba Bailian Wan (CN)', 'per_second', 139900, '1080p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_de7b8852f3bfb25b', 'wan2-5-video', 'Alibaba Bailian Wan (CN)', 'per_second', 42000, '480p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_619943c0e7c80229', 'wan2-5-video', 'Alibaba Bailian Wan (CN)', 'per_second', 83900, '720p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_2ff9cd890779f2e2', 'wan2-5-video', 'Alibaba ModelStudio Wan (INTL)', 'per_second', 42000, '480p', '', '', 5, 'USD', 'https://modelstudio.console.alibabacloud.com', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_4b0dd30bb496a3b3', 'wan2-5-video', 'Alibaba ModelStudio Wan (INTL)', 'per_second', 83000, '720p', '', '', 5, 'USD', 'https://modelstudio.console.alibabacloud.com', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_48f24c21be76549b', 'wan2-6', 'Alibaba Bailian Wan (CN)', 'per_second', 139900, '1080p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_466fa58d44252376', 'wan2-6', 'Alibaba Bailian Wan (CN)', 'per_second', 83900, '720p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_d9b19ccc45d9651b', 'wan2-6', 'Alibaba ModelStudio Wan (INTL)', 'per_second', 139000, '1080p', '', '', 5, 'USD', 'https://modelstudio.console.alibabacloud.com', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_f910b26f338c39b2', 'wan2-6', 'Alibaba ModelStudio Wan (INTL)', 'per_second', 83000, '720p', '', '', 5, 'USD', 'https://modelstudio.console.alibabacloud.com', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_054110f39fb4be89', 'wan2-7', 'Alibaba Bailian Wan (CN)', 'per_second', 139900, '1080p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_9df9f6e0f5a0ae5d', 'wan2-7', 'Alibaba Bailian Wan (CN)', 'per_second', 83900, '720p', '', '', 5, 'USD', 'https://help.aliyun.com/zh/model-studio/model-pricing', 'cn', 1, '2026-07-24T00:00:00Z'),
  ('obs_23bde7bd2f9b494b', 'wan2-7', 'Alibaba ModelStudio Wan (INTL)', 'per_second', 139000, '1080p', '', '', 5, 'USD', 'https://modelstudio.console.alibabacloud.com', 'intl', 0, '2026-07-24T00:00:00Z'),
  ('obs_0b9838418c7dd5cd', 'wan2-7', 'Alibaba ModelStudio Wan (INTL)', 'per_second', 83000, '720p', '', '', 5, 'USD', 'https://modelstudio.console.alibabacloud.com', 'intl', 0, '2026-07-24T00:00:00Z');
