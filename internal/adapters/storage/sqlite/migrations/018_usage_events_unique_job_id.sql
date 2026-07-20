-- 018_usage_events_unique_job_id.sql
--
-- Enforce at most one usage_events row per higgsgo_job_id.
--
-- Before this migration the schema shipped only non-unique secondary
-- indexes (idx_usage_apikey_month / _cpa_month / _group_day / _account_day /
-- _model_day; see 001_init.sql:172-176). That let two independent observers
-- of the same terminal transition — the sync path in core/proxy/service.go
-- and the background pollworker in core/pollworker/worker.go — both call
-- metering.Recorder.OnJobTerminal for the same job, producing duplicate
-- usage_events rows: dashboards and monthly bills would then double-count
-- credits and API-key usage counters would drift.
--
-- The application-level fix (F1) gates metering + webhook + in-flight
-- release on a compare-and-swap on jobs.status: only one observer wins the
-- transition and runs the side effects. This UNIQUE index is defence in
-- depth against that CAS window and against any future path that forgets to
-- gate — the accounting rows themselves are now unique by higgsgo_job_id.
--
-- Note on synthetic "cf_..." ids:
--   The sync path emits a metering event for CreateJob failures with a
--   locally minted id from idgen.NewID("cf") (see e39abb9 and
--   internal/util/idgen/idgen.go). That helper embeds a 12-hex-char
--   random suffix on every call, so distinct create-failures produce
--   distinct ids and this full UNIQUE index — not a partial one — is
--   safe. If that generator ever loses its random suffix, this constraint
--   would surface the collision loudly instead of silently double-billing.
--
-- Pre-index dedupe:
--   Any DB that ran production traffic before F1 landed may hold duplicate
--   rows produced by the exact race this migration is closing. CREATE UNIQUE
--   INDEX would refuse to build the index on those DBs, leaving the ship
--   broken on the deployments most in need of the fix. Collapse each
--   higgsgo_job_id group down to a single canonical row FIRST, then build
--   the constraint.
--
-- Survivor ranking (total order — no ties possible after all four keys):
--
--   1. Terminal-status priority (higher wins):
--        completed = 4   — the definitive success outcome
--        refunded  = 3   — upstream restored credits; still authoritative
--        failed    = 2   — content-level failure
--        timeout   = 1   — we gave up; upstream state unknown
--        anything else = 0 — belt-and-braces for future statuses
--      Rationale: if both observers wrote the row, prefer the higher-quality
--      terminal. A timeout being overwritten by a real completed matches
--      the F1 CAS ordering (completed wins the race in production).
--
--   2. Later ts wins — the observer that finished metering last had the
--      most accurate post-job balance for the delta path.
--
--   3. Higher id (lexicographic) wins — deterministic tiebreak when ts is
--      identical (idgen.NewID uses monotonic hex so this correlates with
--      wall time anyway).
--
--   4. rowid — SQLite's internal row identifier is the absolute last
--      tiebreak; guaranteed distinct so the ranking is total.
--
--   Rows keyed on the sentinel-empty higgsgo_job_id (never emitted by real
--   code) are left alone; the WHERE below intentionally scopes to non-empty
--   ids so we can't accidentally collapse malformed data.
DELETE FROM usage_events
 WHERE higgsgo_job_id != ''
   AND rowid NOT IN (
     SELECT rowid
       FROM usage_events ue1
      WHERE ue1.higgsgo_job_id != ''
        AND NOT EXISTS (
          SELECT 1
            FROM usage_events ue2
           WHERE ue2.higgsgo_job_id = ue1.higgsgo_job_id
             AND ue2.rowid != ue1.rowid
             AND (
                 -- ue2 wins on terminal-status priority.
                 CASE ue2.status
                   WHEN 'completed' THEN 4
                   WHEN 'refunded'  THEN 3
                   WHEN 'failed'    THEN 2
                   WHEN 'timeout'   THEN 1
                   ELSE 0
                 END
                 >
                 CASE ue1.status
                   WHEN 'completed' THEN 4
                   WHEN 'refunded'  THEN 3
                   WHEN 'failed'    THEN 2
                   WHEN 'timeout'   THEN 1
                   ELSE 0
                 END
             OR (
                 -- Same status priority: prefer later ts.
                 CASE ue2.status
                   WHEN 'completed' THEN 4
                   WHEN 'refunded'  THEN 3
                   WHEN 'failed'    THEN 2
                   WHEN 'timeout'   THEN 1
                   ELSE 0
                 END
                 =
                 CASE ue1.status
                   WHEN 'completed' THEN 4
                   WHEN 'refunded'  THEN 3
                   WHEN 'failed'    THEN 2
                   WHEN 'timeout'   THEN 1
                   ELSE 0
                 END
                 AND (
                     ue2.ts > ue1.ts
                     OR (ue2.ts = ue1.ts AND ue2.id > ue1.id)
                     OR (ue2.ts = ue1.ts AND ue2.id = ue1.id AND ue2.rowid > ue1.rowid)
                 )
             )
             )
        )
   );

-- Idempotent (IF NOT EXISTS) and forward-only. Now safe because the DELETE
-- above guarantees at most one row per higgsgo_job_id.
CREATE UNIQUE INDEX IF NOT EXISTS ux_usage_events_higgsgo_job_id
    ON usage_events(higgsgo_job_id);

INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (18, CURRENT_TIMESTAMP);
