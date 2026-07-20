# Changelog

All notable changes to higgsgo are documented here. Format inspired by
Keep a Changelog (keepachangelog.com); versioning will start once we
tag a v0.1.0 release. Until then, everything sits under `[Unreleased]`.

Commits referenced inline as `[hash]` are reachable from `main`; run
`git show <hash>` to inspect the exact diff.

## [Unreleased]

### Video generation alias for new-api / OneAPI (2026-07-21)

`POST /v1/video/generations` (singular `video`) now routes to the same
`HandleVideoGeneration` as the pre-existing `POST /v1/videos/generations`
(plural). new-api / OneAPI-style clients speak the singular form; the
plural form was our own OpenAI-style historical spelling and stays
mounted as a legacy alias — no breaking change for anyone integrated
against `/v1/videos/generations`. Docs and the WebUI code-example
generator now surface the singular path as the preferred one; API
reference, ARCHITECTURE endpoint table and CONVENTIONS naming rule
call out both. Regression coverage: new
`TestPublicRouter_VideoAliasBothPathsMounted` in `internal/api/` sends
an unauthenticated POST to each path and asserts a 401 (auth engaged),
not a 404 (route missing). Note: OpenAI's own current video endpoint
is `POST /v1/videos` with a different body contract — that is not
mounted here.

### Registration Plugin Lands (2026-07-19, post-v0.2)

The registration pipeline moves from "storage-only queue with a
503 admin endpoint" to a full end-to-end signup path under
`-tags register`. All five ROADMAP §5.4 items closed. Public /
reverse-proxy builds are unchanged: default `go build` still
answers 503 `registrar_disabled` and ships none of the automation
code.

#### Plumbing (P4-1, P4-2, P4-3a)

- **SQLite `RegistrationStore`** implementing the full
  `ports.RegistrationStore` interface: Enqueue / NextPending /
  MarkRunning / MarkCompleted / MarkFailed / Get + admin-shaped
  List (newest-first with status/since/limit/offset SQL filters)
  and ResetToPending (for Retry). The `registrations` table has
  been in migration 001 since day one — this commit finally
  reads/writes it. Commits `a854ef7`, `bb21923`.
- **`higgsfield.go` bridge** (under `//go:build register`)
  translates between the main module's int64-id 4-state schema
  and the plugin's string-id 6-state model via a `storeAdapter`.
  The `registrar` facade satisfies `ports.Registrar` by
  delegating each admin call to the store; the optional
  `Start(ctx)` starts a background worker goroutine. Commit
  `31ff48c`.

#### Node subprocess driver (P4-3b)

- **`plugins/register/driver-node/index.mjs`** — a minimal
  Node HTTP server that spawns as a subprocess of higgsgo and
  wraps the higgsfield-register project's `registerAccount()`.
  Endpoints: `GET /ready`, `POST /register`, `POST /shutdown`.
  Lazily imports `../../../higgsfield-register/src/register/
  flow.mjs` so no Playwright / camoufox / DataDome / Graph OTP
  logic is duplicated into higgsgo. Commit `b8899ed`.
- **`plugins/register/adapters/camoufox/driver_node.go`** —
  `NodeDriver` implements `register.Driver` by spawning the
  Node subprocess and talking to it over 127.0.0.1 HTTP. Follows
  the klinggo `browser_client.go` pattern: `Setpgid` process
  group so `Close()` reaps the whole `node → chromium/firefox`
  tree; free-port probe before spawn to fail fast; 30s `/ready`
  poll on startup. Commit `b8899ed`.
- **Higher-level `Driver` interface** on
  `plugins/register/ports.go` — a single `Register(req)` call
  replaces the session-level `Launch → Goto → Fill → Click`
  sequence. `NewFlowWithDriver` wires Flow to delegate one
  round-trip per registration instead of chaining subprocess
  calls per DOM action. Commit `df62cbd`.

#### Account upsert on success (P4-3c)

- **`storeAdapter.MarkCompleted` two-step transition.** When the
  driver returns a `CompletedResult`, the adapter first upserts
  a fully-populated `domain.Account` (cookies marshalled to
  JSON, `PlanType` mapped from string, credits converted to the
  int64-hundredths unit, `Source = "registered"`, `Status =
  active`) and only then flips the `registrations` row to
  `success`. Failure at the upsert bubbles out; the
  `registrations` row stays `running` and Retry re-runs the
  flow idempotently. Nil `AccountStore` degrades cleanly (warn
  + skip upsert). Commit `b7645f2`.

#### Manual bootstrap (documented, not shipped)

`-tags register` builds still need three one-time operator
steps at deploy: (a) sibling `../higgsfield-register` checkout
or `HIGGSFIELD_REGISTER_ROOT`, (b) `npm install` inside
`plugins/register/driver-node/`, (c) `npx playwright install
firefox` for camoufox. Env-var reference for the seven
runtime knobs is in `docs/ROADMAP.md` §5.5. Automating this
into a `scripts/bootstrap-register-driver.sh` is P4-4.

#### Testing

50+ new test cases across the pipeline:

- `internal/adapters/storage/sqlite/registration_store_test.go` —
  6 cases covering lifecycle, fail-path, unknown-id, empty queue,
  list filters + pagination, reset-to-pending.
- `plugins/register/flow_driver_test.go` — 3 cases proving Flow
  delegates to a Driver correctly (happy / fail / ctx-cancel).
- `plugins/register/adapters/camoufox/driver_node_test.go` — 3
  cases: real spawn + `/ready` handshake, missing-script fast
  fail, register smoke test (skipped-by-default because real
  signup is slow/flaky in CI).
- `internal/adapters/registrar/higgsfield/upsert_test.go` — 5
  cases covering the P4-3c mapping matrix (happy path with 12
  field assertions + cookie round-trip, unknown-plan fallback,
  nil-AccountStore degrade, missing-account_id error, upsert-
  failure bubbles without flipping the reg row).

#### Also in this window

- **Card footer button labels restored** on wide accounts cards
  (P2-6's icon-only regression). Uses a `@xs/acccard` container
  query so narrow cards keep icons only. Commit `9f4b83c`.

---

### Audit → Delivery Cycle (July 2026)

A ground-up audit of the pool + admin surface found that many
admin-editable fields were stored, indexed, and exposed through the
WebUI but never consumed at request time — a class of "silent no-op"
bug that turns operator effort into no-op configuration. The cycle
below closes every one of those, plus the two "fake data" surfaces the
audit turned up in the WebUI. See `docs/ROADMAP.md` for the full audit
trail and per-item file:line references.

#### Pool correctness (P0-2, P0-3, P1-4, P1-5, P2-8, P2-9, P3-10, P3-11)
- **`in_flight_jobs` leak fix + boot reconciliation** (P0-2). The proxy's
  three hand-rolled `unlock()` sites are now guarded by `sync.Once` +
  a top-level `defer`, so a panic between `PickAndLock` and any
  release site can no longer strand the counter. Startup runs a
  single `UPDATE accounts SET in_flight_jobs = 0` to reconcile any
  leaks from a prior crash. Commit `cf4000a`.
- **Group `max_concurrent_jobs` + `max_concurrent_per_account` enforcement**
  (P0-3). `PickAndLock` now runs a `SUM(in_flight_jobs)` subquery
  inside the same transaction as the account SELECT+UPDATE, atomic
  under SQLite's serialized writers. `ErrGroupConcurrencyMax` maps
  to HTTP 429 `pool_saturated` (retryable) — distinct from 503
  `no_account_available` (pool is dry). The historical hardcoded
  literal `5` is now parameterised through `PickParams.MaxConcurrentPerAccount`
  with a fallback default. Commit `cf4000a`.
- **Group `allowed_models_regex` / `blocked_models_regex` /
  `monthly_credit_budget` enforcement** (P1-4).
  `proxy.Service.enforceGroupGates` runs the three checks BEFORE
  `PickAndLock`, so a doomed request never consumes an in-flight
  slot. `metering.Recorder.OnJobTerminal` now calls
  `GroupStore.IncrementUsed` at every non-zero terminal so the
  budget gate self-limits against real spend. Invalid regex
  patterns log a WARN and fail open (no 500 from admin
  misconfiguration). Commit `ef33fbc`.
- **Per-account HTTP client honouring `bound_proxy_url`** (P1-5). The
  `utls.Pool` type caches one `*utls.Client` per unique proxy URL
  under a `sync.Mutex`. `upstream.Client.Resolver` is consulted
  before every request; a resolver error is logged and falls back to
  the shared default client so a misconfigured proxy degrades
  gracefully. Before this fix every account shared one egress IP,
  breaking the sticky-IP promise made to Higgsfield at registration
  and inviting Cloudflare / DataDome to correlate accounts. Commit
  `24c19c5`.
- **Jittered LRU tiebreaker on every RouteStrategy** (P2-8). Ordering
  now ends with `, in_flight_jobs ASC, RANDOM() LIMIT 1`. Primary
  sort keys that tie under real load (identical `last_used_at`, same
  `plan_type`, same `priority`) fall through to a least-loaded
  preference then a random tiebreaker. Test seeds three
  otherwise-identical rows and proves 30 picks land on at least 2
  of them — previously all 30 hit row 0. Commit `04bec6c`.
- **Per-Key monthly quota pre-pick gate** (P2-9). `GenerationRequest`
  grew `APIKeyMonthlyQuota` + `APIKeyMonthlyUsed`; `/v1/videos` and
  `/v1/images` forward them from the middleware context.
  `proxy.Service.enforceKeyGates` returns
  `domain.ErrAPIKeyQuotaExceed` (HTTP 402 `quota_exhausted`) before
  the pool spends a slot on a doomed request. Zero quota preserves
  the historical "unlimited" default. Commit `1c588f7`.
- **Cross-group spillover for multi-binding API keys** (P3-10). Keys
  bound to multiple groups used to 400 with `ambiguous_group`; now
  `resolveGroup` returns the ordered candidate list sorted by group
  name ascending (`primary`, `fallback-1`, `fallback-2`).
  `proxy.Service.Generate` iterates and falls over on
  `ErrGroupConcurrencyMax` / `ErrGroupQuotaExhausted` /
  `ErrNoEligibleAccount` / model regex mismatch. `req.GroupID` is
  rewritten to the group that actually served the pick so the Job
  row, metering event, and webhook reflect where the credits landed.
  Non-spillover-eligible errors (per-key quota, upstream errors)
  short-circuit. Commit `feec5d8`.
- **Async job in_flight tracking across the full lifecycle** (P3-11).
  The async branch now hands ownership of the `in_flight` slot to
  the pollworker instead of releasing it at `CreateJob` return.
  `pollworker.Worker.releaseInFlight` fires at every terminal path
  (successful transition, timeout, `markTimeout`); the
  fetch-terminal stall path deliberately does NOT release so a
  transient `FetchJob` failure doesn't oversubscribe on retry. This
  makes the group + per-account concurrency caps (P0-3) enforce
  across BOTH sync and async work — previously an async video
  burst could oversubscribe an account because its slots freed at
  `CreateJob` time even though upstream jobs were still running.
  Commit `50142d3`.

#### WebUI honesty (P0-1, P2-6, P2-7, P3-13)
- **Per-group member priority editable in the WebUI** (P0-1). The
  `account_group_members.priority` column was read by `PickAndLock`
  under `route_strategy = priority` since day one, but the UI had
  no way to see or change it — every membership landed on the DB
  default of 100. Added `ListMembersWithPriority` to the
  `GroupStore` port and a `members_detail` field on the group
  members endpoint; the account edit dialog now shows a per-group
  priority input beside each selected group and upserts on save.
  Commit `32aa2a5`.
- **Real account probe endpoint** (P2-6). Replaced the empty toast-
  only "health" button with `POST /admin/accounts/{id}/probe` — an
  active `upstream.FetchWallet` call that exercises the JWT
  minter, the per-account HTTP client (P1-5), and the JA3
  fingerprint the pool actually uses in production. Response is
  200 for BOTH success and failure with `ok`/`latency_ms`/`balance`
  or a classified `error.kind` (unauthorized / forbidden /
  rate_limit / upstream_5xx / timeout / network / internal). 15 s
  outer deadline. Nil-Prober answers 503 `probe_disabled` so the
  WebUI can distinguish "not configured" from "call failed".
  Commit `7b9357a`.
- **Mock uptime removed** (P2-7). The `mockUptime()` helper that
  fabricated 95-100% uptime for every model with no
  `/admin/model-health` row is gone. Rows without real data show an
  em dash + muted "no data" bar; the detail sheet says
  "No probe data yet — run the regression ticker to populate".
  `generateMockSlots` was renamed to `generateEmptySlots` and now
  produces `total=0` slots that render as muted gray with "No data"
  tooltips. Fabricated confidence is worse than a blank field —
  operators use this tool to decide which accounts to disable.
  Commit `d2c3fc6`.
- **Per-slot time-series probe data** (P3-13). The `model_health`
  table was already storing per-check verdicts; we just weren't
  reading them as a time series. `ModelHealthStore.SlotsByJST` +
  `GET /admin/model-health/{jst}/slots` bucket them into
  fixed-width slots aligned to the current-slot top. The detail
  sheet's uptime bar consumes real slots (`generateEmptySlots`
  fallback for freshly-added models). Table view keeps the
  aggregate percentage — no N+1 fetch. Commit `cb87a47`.

#### Documentation refresh
- **`docs/ROADMAP.md`** created as the single source of truth for
  what is actually wired vs stored-only, with `file:line` evidence
  and a P0/P1/P2/P3 fix list. Every audit finding tracks through
  this file from "found" to "landed".
- **`docs/POOL-AND-CPA.md`** §2.3 pick-logic SQL and §7.2 route
  strategies corrected: `round_robin` is documented as LRU (its
  actual behaviour), `least_used` sorts by lifetime consumed
  credits (not month-to-date as the pre-audit doc claimed), and
  every silent no-op field has a status marker.
- **`docs/ARCHITECTURE.md`** §8 sticky-proxy claim marked as ❌ not
  wired before P1-5 landed, then rewritten as ✅ live once it did.
  Added §11a covering failover, bearer, settings, model overrides,
  version endpoint, resolver, playground; added §2.0 documenting
  the monorepo module split.
- **`docs/PLUGGABLE.md`** rewrote §0 for the monorepo split
  (`go.work`, `-tags register`, module-path alignment).
  Retained the in-module Provider abstraction as §§1-8.
- **`docs/API_REFERENCE.md`** backfilled 12 previously-undocumented
  admin endpoint groups (failover, model_overrides, routing_settings,
  settings/bearer, registrations, audit export, tickers, version,
  webhooks, playground, plus the P2-6 probe + P3-13 slots surfaces).
- **`docs/OPERATIONS.md`** added zero-downtime bearer rotation, the
  failover console, runtime model overrides, audit export, and bulk
  account import/export runbooks.
- **`docs/STACK-DECISIONS.md`** added the WebUI stack section
  (React 19 + Vite 7 + TanStack + shadcn + i18next + tabler-icons)
  and the monorepo split rationale (Go workspace + build tags over
  plugins / wasm / RPC).

#### WebUI (React admin console)
- **React admin console scaffolding** landed in-tree at `webui/`, embedded
  in the Go binary via `//go:embed`. Vite 7 + React 19 + TanStack
  Router/Query + shadcn/ui + i18next. Everything below runs against the
  admin API surface. Commits [42f2bb2], [939d06a], [d81f07f].
- **Dashboard** — KPI trends, pool composition (aggregated from
  `/admin/stats/pool`), usage charts. 30 s refetch. Commit [e0503b5].
- **Accounts page** — table + card views with sortable columns, group
  membership pill row (`+N` overflow), inline pause/resume switch, edit
  dialog with proxy / priority / max_concurrent / note / groups. Card
  grid scales 1→2→3→4 cols via container queries. Commits [6f2dc09],
  [3da2a12] (fix: PATCH group members via `/members` endpoint, not
  `/bindings`), [4104748] (responsive card layout with overflow-safe
  tags).
- **Keys page** — create / edit dialog, group bindings, per-key stats.
  Commit [aec3f54].
- **Groups page** — edit dialog, model multi-select, stats hook. Commit
  [b3382f1].
- **Playground** — credits warning, cost tag, param-form i18n against
  `/v1/playground/{models,estimate,execute}`. Commit [7d76aaf].
- **Models page** — uptime bar, code examples, detail sheet. UI still
  falls back to mock uptime when `/admin/model-health` has no data for a
  JST; see `docs/ROADMAP.md` P2-7. Commit [2e50353].
- **Jobs page** — currently seeded with mock data for visual QA (real
  wire pending). Commit [da3228f].
- **CPA plugin / registrations / settings pages** shipped for admin
  surfaces that already exist server-side. Commit [d6e336d].
- **i18n coverage** — English + Chinese for every page. Commit [4f33f26].

#### Admin API additions
- **`PATCH /admin/keys/{id}`** and **`GET /admin/keys/{id}/stats`** +
  **`GET /admin/keys/{id}/groups`**, plus
  **`GET /admin/accounts/{id}/eligible-models`** to power the WebUI
  key/account editors without additional round-trips. Commit [305e406].
- **`Account.Priority`, `APIKey.Kind`, `APIKey.KeyLast4`** landed in
  domain + storage, backed by migrations `010` (accounts.priority
  column + index) through `012`. `priority` is the sort key when a
  group's `route_strategy = priority` (`account_store.go:475`). Commit
  [91d25b7].
- **Playground surface** — `api_keys.playground_scope` + three
  endpoints under `/v1/playground/` gated by a dedicated middleware
  (`internal/api/middleware/playground.go`). Commit [35bbbc8].
- **`/admin` prefix mount + PUT/GET group bindings** — normalizes route
  layout so every admin verb lives under `/admin/*`. Commit [37e0d46].

#### Documentation
- **`docs/ROADMAP.md`** created — single source of truth for what is
  actually wired vs stored-only, with `file:line` evidence and a
  P0/P1/P2 fix list.
- **`docs/POOL-AND-CPA.md`** — pick-logic SQL, group concurrency, and
  route strategy sections corrected to match code
  (`account_store.go:395-503`). Removed the false claim that
  `least_used` sorts by monthly credits — it's lifetime consumed.
- **`docs/ARCHITECTURE.md`** — §8 sticky-proxy claim marked as ❌ not
  wired (all upstream calls still share the process-level
  `HIGGSGO_UPSTREAM_PROXY_URL`). Added §11a covering failover, bearer,
  settings, model overrides, version endpoint, resolver, playground.
  Added §2.0 documenting the monorepo module split.
- **`docs/PLUGGABLE.md`** — added §0 documenting the monorepo
  module split (public slim build vs `-tags register` full build) and
  §10 wiring status. Kept the Provider abstraction body for
  `internal/adapters/*`.

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
- **Audit middleware extended to `/internal/*`** so CPA plugin
  writes land in `audit_events` too. The lookup table maps
  `/internal/register` and `/internal/{partner_id}` to
  `resource_type=cpa_partner`, `/internal/execute` to
  `cpa_execute`, `/internal/registrations/{id}` to
  `cpa_registration`. Commit [1b8427e].

#### Key lifecycle
- **`/admin/keys/{id}/rotate|pause|resume|reset_usage`** POST
  endpoints back the four missing key states operators previously
  had to reach with raw SQL. Rotate mints a fresh `sk-hg-<40hex>`
  and returns the plaintext once; pause and resume flip
  `active` <-> `paused` (revoked stays terminal); reset_usage
  zeros `monthly_used` without touching the quota. The
  APIKeyAuth middleware now rejects paused keys with 401
  `api_key_paused` and revoked with 401 `api_key_revoked` so
  clients can branch on the two. Migration 008 adds
  `api_keys.updated_at` so operators can order by last-modified.
  Commit [7da0839].

#### Registry hot-reload
- **`POST /admin/models/reload`** reloads
  `data/reference/verified-models.json` in place. Response
  includes `previous_count` / `current_count` for a quick "did it
  pick up the new file" sanity check. Wrapped in a 30 s ctx;
  errors surface as `500 reload_failed`. Nil registry -> 503.
  Commit [df7e130].

#### Account lifecycle
- **`POST /admin/accounts`** three-format import: `session_paste`
  (direct harvested fields), `higgsfield_register_json` (paste the
  output/*.json from higgsfield-register verbatim), `raw_cookies`
  (Chrome DevTools Cookie header — reverses session_id out of
  clerk_active_context / __client / sess_ prefix). Conflict path
  returns 409 with a `?upsert=true` escape hatch.
- **`GET /admin/accounts/export`** streaming in JSON array, JSONL,
  or CSV. `include_secrets` defaults to false so casual snapshots
  cannot leak session_id / cookies to a local disk. Batching is
  500-row chunked + `http.Flusher` so a large window stays
  memory-bounded. Commit [f9cac22].

#### Audit trail (part 2)
- **`GET /admin/audit/export`** streaming JSONL (default) / CSV
  with the same `?since` / `?until` / `?actor` / `?resource_type`
  / `?resource_id` / `?method` / `?limit` filter set as the list
  endpoint. `Content-Disposition` attaches
  `audit-<since>-<until>.<ext>` so browsers download rather than
  render. Commit [df97d8c].

#### Reliability (part 2)
- **`monthreset` ticker** zeros `api_keys.monthly_used` at each
  UTC calendar month boundary. Calendar mode sleeps to the next
  boundary + 5min slack; polling mode (Interval > 0) drives the
  same reset from a short cadence, gated by month-of-clock so it
  neither misses nor spams. On by default because a stale
  `monthly_used` silently freezes traffic on the first of the
  month. Commit [d9d62c2].

#### CLI (part 2)
- **`higgsgo-cli pause-key / resume-key / reset-usage`** round out
  the CLI's mirror of the `/admin/keys/{id}/*` write surface.
  Rotate + disable were already there. Each subcommand goes
  through `ports.APIKeyStore` and emits JSON on stdout for shell
  pipelines. Commit [29d84db].

#### WebUI unblock
- **CORS middleware** on `/admin/*` with the WebUI's dev origins
  wired in `higgsgo.dev.toml`. Echoes the request Origin (never
  `*`) so credentials still work; preflight OPTIONS short-circuits
  before `BearerAuth` would 401 it. Empty allowlist keeps the
  middleware a pass-through, so same-origin deploys are
  unaffected. Commit [6702d3f].

### End-to-end
- First hermetic e2e test: admin create key -> v1 image
  generation -> job list (scoped and admin) -> /admin/usage row
  -> `/admin/jobs/purge` -> post-purge assertions. httptest.Server
  mocks `fnf.higgsfield.ai`; `svc.AsyncByDefault=false` keeps the
  sync path deterministic. Runs in ~0.5 s under
  `go test ./internal/e2e/`. Commit [28dca21].

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
