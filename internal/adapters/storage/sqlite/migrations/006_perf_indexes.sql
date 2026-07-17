-- 006_perf_indexes.sql
--
-- Composite indexes for list / purge / usage-query hot paths.
--
-- Motivation: with ~1k+ jobs the /admin/jobs, /v1/jobs, and /admin/usage
-- list handlers were falling back to full-table scans because the
-- migration-001 indexes were single-column or did not embed the ORDER BY
-- direction. Adding tail-order composites lets SQLite's planner satisfy
-- both the WHERE clause and the ORDER BY DESC out of the index without
-- materialising a temp b-tree.
--
-- Column names verified against 001_init.sql / 002_jobs_indexes.sql:
--   jobs(api_key_id, account_id, status, request_ts, finished_at)
--   usage_events(api_key_id, ts, billing_day, model_alias)
--
-- Purge (job_store.Purge) filters `status IN (...) AND finished_at < ?`,
-- so the status/finished_at composite here matches the WHERE order used
-- by the planner (see internal/adapters/storage/sqlite/job_store.go).

-- jobs: /admin/jobs and /v1/jobs list paths (ORDER BY request_ts DESC scoped by api_key_id).
CREATE INDEX IF NOT EXISTS idx_jobs_api_key_request_ts
    ON jobs(api_key_id, request_ts DESC);

-- jobs: /admin/jobs?account_id=... direct scans.
CREATE INDEX IF NOT EXISTS idx_jobs_account_request_ts
    ON jobs(account_id, request_ts DESC);

-- jobs: /admin/jobs/purge status IN (...) AND finished_at < ?.
CREATE INDEX IF NOT EXISTS idx_jobs_status_finished
    ON jobs(status, finished_at);

-- usage_events: /admin/usage?api_key_id=... direct scans (ORDER BY ts DESC).
CREATE INDEX IF NOT EXISTS idx_usage_api_key_ts
    ON usage_events(api_key_id, ts DESC);

-- usage_events: Aggregate GROUP BY billing_day.
CREATE INDEX IF NOT EXISTS idx_usage_billing_day
    ON usage_events(billing_day);

-- usage_events: Aggregate GROUP BY model_alias.
CREATE INDEX IF NOT EXISTS idx_usage_model_alias
    ON usage_events(model_alias);

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (6, CURRENT_TIMESTAMP);
