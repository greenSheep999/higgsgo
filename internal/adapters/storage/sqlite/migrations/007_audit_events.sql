-- 007_audit_events.sql
--
-- audit_events: append-only trail of admin write operations.
--
-- Motivation: every /admin/* POST/PUT/PATCH/DELETE (key create, account
-- pause, CPA register, job purge, ...) previously left no forensic
-- record. When production breaks we cannot see who mutated what.
-- The middleware/audit chain now records one row per admin write.
--
-- Column notes:
--   * actor          — first 8 chars of the bearer token, or "anonymous"
--                      when the request had no Authorization header. Full
--                      tokens are secrets and never persisted.
--   * route          — chi RoutePattern (e.g. /admin/keys/{id}), stable
--                      under URL param variation.
--   * resource_type  — derived from route via a small lookup table:
--                      apikey / account / group / job / partner / etc.
--   * resource_id    — chi.URLParam value pulled from the matched
--                      route (e.g. the {id} in /admin/keys/{id}).
--   * body_hash      — SHA-256 hex of the request body. We hash rather
--                      than store to avoid persisting secrets (new API
--                      key names, group configs). Same body hash across
--                      rows implies retry / replay.
--   * error_detail   — reserved for future non-2xx annotations; empty
--                      today, still cheap to keep the schema symmetric.

CREATE TABLE IF NOT EXISTS audit_events (
    id            TEXT PRIMARY KEY,
    ts            TEXT NOT NULL,                   -- RFC3339 UTC
    actor         TEXT NOT NULL,                   -- bearer token prefix (8 chars) or "anonymous"
    method        TEXT NOT NULL,                   -- HTTP method
    path          TEXT NOT NULL,                   -- request URL path
    route         TEXT NOT NULL,                   -- chi RoutePattern
    status        INTEGER NOT NULL,                -- HTTP status code
    resource_type TEXT NOT NULL DEFAULT '',        -- derived from route
    resource_id   TEXT NOT NULL DEFAULT '',        -- extracted from path param
    body_hash     TEXT NOT NULL DEFAULT '',        -- SHA-256 hex
    error_detail  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_audit_events_ts
    ON audit_events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_actor
    ON audit_events(actor, ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_resource
    ON audit_events(resource_type, resource_id, ts DESC);

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (7, CURRENT_TIMESTAMP);
