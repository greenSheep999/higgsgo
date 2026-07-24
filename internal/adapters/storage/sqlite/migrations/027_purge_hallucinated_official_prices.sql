-- 027_purge_hallucinated_official_prices.sql
--
-- Purges the 34 official_price_observations rows seeded in migration 023
-- (kling3_*, kling3t_*, and 024's op_* rows for kling-2.6, seedance-*,
-- veo*, sora2-video, minimax-hailuo, grok-video*). Those seeds were
-- written without a verified upstream source and have been recognised
-- as fabricated data (see feedback-never-fabricate-seed-data memory).
--
-- Verified rows will be re-inserted in migration 029 keyed by the raw-
-- pricing/*.md documents the operator maintains in higgsfield-register.
-- Purge is unconditional: no verified row shares an id with the seed
-- ids we know we wrote (kling3_*, kling3t_*, op_*).

DELETE FROM official_price_observations
 WHERE id LIKE 'kling3_%'
    OR id LIKE 'kling3t_%'
    OR id LIKE 'op_%';

INSERT OR IGNORE INTO schema_versions (version, applied_at)
  VALUES (27, CURRENT_TIMESTAMP);
