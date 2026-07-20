-- 019_simplify_route_strategy.sql
--
-- Route strategy is being collapsed from 6 options to 2:
--
--   load_balance (round_robin) — the new default. Auto-cheap-first
--       (tier ASC) + LRU + least-loaded + jitter, all in one SQL
--       ORDER BY. Absorbs the semantics of best_fit / cheapest_first
--       / least_used / most_credits_first.
--   priority                     — explicit ordering via
--       accounts.priority DESC.
--
-- Legacy per-group values migrate to round_robin because their
-- behaviour is now the load_balance default. `priority` is preserved.
-- This is data-only migration; the SQL builder in account_store.go
-- keeps case branches for the legacy values so downgraded / partially
-- migrated deployments don't break.
UPDATE account_groups
   SET route_strategy = 'round_robin'
 WHERE route_strategy IN ('best_fit', 'cheapest_first',
                          'least_used', 'most_credits_first');

INSERT OR IGNORE INTO schema_versions (version, applied_at)
  VALUES (19, CURRENT_TIMESTAMP);
