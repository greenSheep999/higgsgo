# Changelog

All notable changes to higgsgo are documented here. Format inspired by
Keep a Changelog (keepachangelog.com); versioning will start once we
tag a v0.1.0 release. Until then, everything sits under `[Unreleased]`.

Commits referenced inline as `[hash]` are reachable from `main`; run
`git show <hash>` to inspect the exact diff.

## [Unreleased]

### Added

#### Metering
- **End-to-end metering pipeline** wiring every terminal job to a
  `usage_events` row, a JWT refresher goroutine that keeps the upstream
  Higgsfield session warm, an outbound webhook dispatcher for partner
  notifications, and the first cut of admin surfaces to inspect state.
  This is the backbone that turns the proxy from "forward and forget"
  into a billable, observable service. See commit [058a4e4].
- **Markup pass-through** so each API key's `MarkupPct` flows from the
  auth layer through to the `Recorder`, letting one deployment charge
  different partners different rates without code changes. Commit
  [10be4c8].
- **Prometheus `UsageCredits` counter** incremented on every terminal
  job, labeled by model and outcome, so revenue can be graphed next to
  latency and errors from the same scrape. Commit [6a00641].

#### Admin API
- **`/admin/jobs` unfiltered listing** exposing every internal field
  (`created_by`, `group_id`, `markup_pct`, upstream ids) for operator
  triage — distinct from `/v1/jobs` which is scoped to the caller.
  Commit [3461839].
- **`/admin/model-health` read surface** surfacing the regression
  ticker's latest verdict per model so an on-call engineer can answer
  "is `wan2_2_animate` broken right now?" without opening SQLite.
  Commit [bda64cf].
- **Groups CRUD at `/admin/groups`** backed by a sqlite `GroupStore`,
  with group-scoped pick so a group's accounts are the only ones
  eligible when a caller's key resolves into that group. Commit
  [c8f5eeb].

#### V1 API
- **`GET /v1/jobs`** returning the caller's own jobs (filtered by API
  key), giving downstream integrations a way to reconcile without
  polling individual job ids. Commit [61dffe8].
- **Automatic `group_id` resolution** from the caller's API key
  bindings when the request omits it, so a partner that only ever uses
  one group never has to send the field. Commit [245737e].
- **Per-API-key token-bucket rate limiter** on `/v1/*`, sized from the
  key record itself so a noisy partner can be throttled without
  redeploy. Commit [6eaa498].

#### CPA plugin mode (Mode B)
- **`/internal/*` route family** enabling the CPA-plugin deployment
  where higgsgo runs behind a partner's own gateway; the internal
  routes trust an upstream header instead of the public API key check.
  Commit [d93642f].
- **Schema-backed CPA partner id** replacing the earlier "prefix on
  `created_by`" hack with a real column, so partner attribution is
  queryable and can't collide with user-generated ids. Commit
  [52db4c4].

#### Observability
- **Prometheus `/metrics` endpoint** exposing HTTP request counters,
  account-pool gauges, and usage counters from a single scrape target,
  so Grafana dashboards can be wired up without extra exporters.
  Commit [477bb81].
- **Upstream request latency histogram** labeled by upstream endpoint
  and status, letting us tell apart "Higgsfield is slow" from "our
  proxy is slow" on a single panel. Commit [56d2045].
- **Structured access log via `slog`** with health-probe paths skipped,
  cutting log volume and making JSON ingest into Loki/ELK a one-liner.
  Commit [36bedc3].

#### Reliability
- **Regression ticker with sqlite `ModelHealth` store** that walks the
  model list oldest-first once per day, records pass/fail per model,
  and feeds `/admin/model-health`. Catches upstream regressions before
  a customer reports them. Commit [b16816b].
- **`higgsgo-cli` read/write subcommands** for operational tasks
  (inspect keys, flip flags, force a health check) so on-call work
  doesn't require writing ad-hoc SQL. Commit [574b2fa].

#### Housekeeping
- **`JobStore.Purge`** + `POST /admin/jobs/purge` for reclaiming
  terminal-state rows once accounting has flowed into `usage_events`.
  Empty `statuses` is a no-op guard so a mis-configured caller cannot
  wipe every finished job by omitting the filter. Commit [fe3446c].

#### Direct group binding
- **`api_keys.group_id`** column (migration 005) lets a CPA-scoped key
  route 1:1 to a pool group without a JOIN through
  `apikey_group_bindings`. The M:N binding table stays as the general
  case; the direct column wins when both are set. Commit [c324d20].

#### Webhook observability
- **`webhook.Dispatcher` counters** (`enqueued` / `delivered` /
  `failed` / `dropped` / `in_flight`) plus `GET /admin/webhooks/stats`
  so operators can answer "are callbacks flowing?" without tailing
  logs. Counters are `sync/atomic` on the hot path. Commit [7f2328f].

#### Upstream reliability
- **Per-endpoint request timeouts** on the upstream client, tuned to
  the real fnf.higgsfield.ai traffic (90 s for `create_job`, 15 s for
  the small GETs). Config knob is `[upstream.timeouts]`; the transport
  ceiling from utls is unchanged. Commit [66e5690].

#### Performance
- **Composite indexes on `jobs` + `usage_events`** (migration 006)
  matching every hot query path: `(api_key_id, request_ts DESC)`,
  `(account_id, request_ts DESC)`, `(status, finished_at)` on
  jobs; `(api_key_id, ts DESC)`, `(billing_day)`, `(model_alias)`
  on usage_events. EXPLAIN QUERY PLAN confirms the planner picks
  them for /v1/jobs, /admin/jobs, /admin/usage. No store code
  changed — indexes are transparent. Commit [366f2c0].

#### V1 filtering
- **`/v1/models` filters + pagination**: `output`, `requires_paid`,
  `requires_unlim`, `q` (case-insensitive alias substring),
  `include_unstable`, `include_deprecated`, plus `limit` /
  `offset`. Response echoes `total_before_pagination` so the
  caller can decide whether to fetch the next page without
  guessing. `?tier=` intentionally not exposed — gating lives on
  individual booleans; combine `requires_paid` + `requires_unlim`
  instead. Commit [002eea9].

#### Manual ticker triggers
- **`POST /admin/tickers/refresher`** and **`/admin/tickers/regression`**
  force one pass immediately, wrapped in a 30 s context timeout.
  Nil runner returns 503 unavailable. Refresher and regression
  ticker both grow `TriggerOnce(ctx)` exported wrappers around
  the existing private `tick`. Commit [477252a].

#### Audit trail
- **`audit_events` table** (migration 007) + middleware that wraps
  the admin router group after `BearerAuth`. Every POST / PUT /
  PATCH / DELETE lands one row with actor (first 8 chars of
  Bearer), method, path, chi route pattern, status, resource
  type, resource id, and SHA-256 hash of the request body. Raw
  body is never persisted. Insert runs in a fresh goroutine with
  a detached 5 s ctx so accounting can never block the response.
- **`GET /admin/audit`** exposes the trail with `since` / `until`
  / `actor` / `resource_type` / `resource_id` / `method` filters
  matching the AuditFilter port. Same response shape as
  `/admin/usage`. Commit [c773bc5].

#### Documentation
- **Operations runbook + API reference** covering deploy, backup,
  rate-limit tuning, and every public endpoint's request/response
  shape — the first docs written for someone who is not the author.
  Commit [7b681eb].
- **This CHANGELOG** landed in commit [9e99d9a] and is kept up to
  date as commits merge.

### Changed
- **Upstream call path self-heals on 401** by invalidating the cached
  Higgsfield JWT and retrying the request exactly once, so an expired
  session no longer surfaces to callers as a 5xx. Commit [5f862e2].
- **Access log format** moved from ad-hoc `fmt.Printf` lines to
  `slog`-structured JSON with health-probe paths (`/healthz`,
  `/metrics`) skipped. Commit [36bedc3].
- **CPA partner attribution** migrated off the `created_by=cpa:...`
  prefix convention onto a first-class schema column, so old rows will
  be backfilled by the next migration. Commit [52db4c4].

### Fixed
- **Stale JWT causing spurious 401s** on the upstream Higgsfield API;
  the client now invalidates and refreshes the token on a single 401
  and retries transparently. Commit [5f862e2].

### Tests
- **Coverage for metering, refresher, webhook dispatch, admin
  endpoints, and the poll worker** landed alongside the pipeline in
  commit [c9454c2], giving the new code an executable spec before it
  saw real traffic.

[Unreleased]: https://github.com/higgsgo/higgsgo/compare/524ea37...HEAD
