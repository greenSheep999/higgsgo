# higgsgo Roadmap (Implementation Status + Priority)

> Last updated: 2026-07-18 after full audit sweep across pool / concurrency /
> priority / proxy / load-balancing / registrations / failover.
>
> This document is the **single source of truth for what actually works vs
> what is only defined on disk**. Every claim carries a `file:line`
> reference so you can verify it in one click. When code diverges from this
> doc, update this doc first.

---

## 1. Snapshot: What is Actually Wired

### ✅ Fully wired in the request path

| Feature | Where it lives | Notes |
|---|---|---|
| API Key → Group resolve | `internal/api/v1/handler.go:318-352` | 3-tier: explicit body > `api_key.group_id` > `apikey_group_bindings` |
| Group → Account filter | `internal/adapters/storage/sqlite/account_store.go:435-438` | `WHERE id IN (SELECT account_id FROM account_group_members ...)` |
| `RouteStrategy` ORDER BY | `account_store.go:445-479` | 5 strategies: `round_robin` / `least_used` / `cheapest_first` / `most_credits_first` / `priority` |
| `Account.Priority` sort | `account_store.go:475` | Only under `RoutePriority` strategy |
| `account_group_members.priority` sort | `account_store.go:471-473` | Backend reads it; WebUI cannot edit yet |
| Failover / disable / throttle / recover | `internal/core/failover/*`, `internal/api/admin/failover.go`, `webui/src/components/settings/failover-dialog.tsx` | Full stack, controller + recoverer goroutines wired in `main.go:144-153` |
| Admin bearer runtime rotation | `internal/core/bearer/manager.go`, `internal/api/admin/settings.go`, migration `014` | Rotate without restart |
| Version check | `internal/version/`, `internal/api/admin/version.go` | GitHub release compare, 1h cache |
| Model overrides | migration `015`, `internal/adapters/storage/sqlite/model_override_store.go`, `internal/api/admin/model_overrides.go` | Reload triggers `Registry.Reload()` |
| Default RouteStrategy setting | `internal/api/admin/routing_settings.go` | Affects new-group defaults only |
| Model health snapshot | `internal/api/admin/model_health.go`, regression ticker | UI has mock uptime fallback (see gaps) |
| Pool composition | `/admin/stats/pool` + `webui/src/components/dashboard/pool-composition.tsx` | Aggregates DB state, 30s refetch |

### ⚠️ Defined but only partially wired

| Feature | State | Missing |
|---|---|---|
| `account_group_members.priority` | Backend reads it; WebUI now exposes per-group priority in the account edit dialog (P0-1 landed). `addGroupMember` on the backend is an upsert (ON CONFLICT DO UPDATE), so the same call updates existing priorities. | — |
| `internal/core/resolver/` (config cascade) | Correct implementation of Key > Group > Account > Global precedence for concurrency / proxy / route / budget / regex / rate limit / markup | **Zero external importers** — never called from proxy path; see §3 |

### ❌ Stored but never enforced (silent no-op fields)

Fields you can set in the DB / admin API / WebUI that **have no runtime effect**:

| Field | Storage | Actual runtime behavior |
|---|---|---|
| `account.bound_proxy_url` | `account_store.go:62,175,562` | ✅ **Enforced** (P1-5 landed). `utls.Pool` (`internal/adapters/httpclient/utls/pool.go`) caches one `*Client` per unique proxy URL and is wired as `upstream.Client.Resolver` in `main.go`. Empty `bound_proxy_url` falls back to the process-level `HIGGSGO_UPSTREAM_PROXY_URL`. |
| `group.max_concurrent_jobs` | `group_store.go` | ✅ **Enforced** (P0-3 landed). `proxy.Service.resolveGroupPolicy` reads it and passes `MaxGroupInFlight` to `PickAndLock`, which runs a `SUM(in_flight_jobs)` subquery inside the tx and returns `ErrGroupConcurrencyMax` when tripped. 429 `pool_saturated` at the HTTP layer. |
| `group.max_concurrent_per_account` | `group_store.go` | ✅ **Enforced** (P0-3 landed). Same policy hop feeds `MaxConcurrentPerAccount` into `PickAndLock` WHERE. Falls back to the historical `5` when the group hasn't set a value. |
| `group.allowed_models_regex` | `group_store.go` | ✅ **Enforced** (P1-4 landed). `proxy.Service.enforceGroupGates` runs the compiled regex against the resolved alias before pick; miss → `ErrModelNotAllowed` → HTTP 403 `model_not_allowed`. Invalid pattern logs WARN and fails open. |
| `group.blocked_models_regex` | `group_store.go` | ✅ **Enforced** (P1-4 landed). Same gate; match → `ErrModelBlocked` → HTTP 403 `model_blocked`. Blocked wins over allowed when both patterns match. |
| `group.monthly_credit_budget` / `monthly_credit_used` | `group_store.go` | ✅ **Enforced** (P1-4 landed). Pre-pick gate compares `MonthlyCreditUsed + EstCost` against `MonthlyCreditBudget`; over → `ErrGroupQuotaExhausted` → HTTP 402 `group_budget_exhausted`. `metering.Recorder.OnJobTerminal` now calls `GroupStore.IncrementUsed` at every non-zero terminal so the counter actually climbs. Zero budget disables the gate. |
| `account.max_concurrent` | migration `016` | **Ignored.** Never read by pick path. |
| `AvailableSlots()` | `domain/account.go:140-149` | Const `upstreamLimit = 6` diverges from the enforced literal `5`. |
| `cfg.Pool.MaxInFlightPerAccount` | `config.go:227,329` | **Ignored** by pick path. Only surfaced by config parser. |

### UI-only placeholders (resolved)

| Feature | UI location | Backend state |
|---|---|---|
| Account "probe" button | `webui/src/routes/accounts.tsx` | ✅ **Real** (P2-6 landed). `POST /admin/accounts/{id}/probe` calls `upstream.Client.FetchWallet` — exercises JWT mint + per-account proxy + TLS fingerprint. Response body carries `ok`/`latency_ms`/`balance` on success, or a classified error kind on failure (unauthorized / forbidden / rate_limit / upstream_5xx / timeout / network / internal). Handler always mounted; nil Prober answers 503 `probe_disabled` so the WebUI can tell "not configured" from "call failed". |
| Per-model uptime bars | `webui/src/routes/models.tsx` | ✅ **Honest** (P2-7 landed). No more `mockUptime` backfill. Rows without a `/admin/model-health` row show a muted "no data" bar + em-dash percentage; the detail sheet says "No probe data yet — run the regression ticker to populate". Real time-series probe data (per-slot success/fail counts) is still a P3 backend addition. |

---

## 2. Load-Balancing Reality Check

### 2.1 The names lie

- `round_robin` is **least-recently-used**, not round-robin. `ORDER BY last_used_at ASC LIMIT 1`. Under bursty concurrent picks the same row wins until the surrounding transaction commits — hot-spot risk.
- `least_used` sorts by lifetime consumed credits (`total_plan_credits - subscription_balance`), not month-to-date, despite what `POOL-AND-CPA.md` said in v0.3.

### 2.2 There is no inter-group balancing

`resolveGroup` picks **one** group up front and passes it as a hard filter. If you want spillover between groups, that logic doesn't exist yet.

### 2.3 No fair-share primitives

No weighted round-robin, no least-connections, no lease reservation, no in-memory counters. Everything is a single SQL `ORDER BY ... LIMIT 1` with `last_used_at` as tie-breaker.

**Cheapest upgrade** (deferred): seed an in-memory atomic pick-count table on process start (from `in_flight_jobs`), consult it as a jittered tiebreaker inside the transaction. No schema change required.

---

## 3. Config Cascade — the missing engine

`internal/core/resolver/resolver.go` implements exactly the precedence rule
we want:

```
MaxConcurrent : Key > Group.PerAccount > Account > Global
ProxyURL      : Account > Global
RouteStrategy : Group > Global (default round_robin)
MonthlyBudget : Key.MonthlyQuota > Group.MonthlyCreditBudget
Allowed/BlockedModels : Group only
RateLimit     : Key > Group > Global
MarkupPct     : Key only
```

`grep -rn "core/resolver"` currently returns **zero external importers** in
production code. The package compiles, tests pass in isolation, and it is
the single correct source of truth for six of the "stored but never
enforced" fields listed in §1. **Decision: wire it in, not delete it.**

Target call site: `internal/core/proxy/service.go` between `resolveGroup`
(v1/handler.go) and `Store.PickAndLock`. `Resolve()` returns a
`ResolvedConfig`; extend `PickParams` (`internal/ports/storage.go:85-99`)
to accept the fields the SQL WHERE / ORDER BY layer needs.

---

## 4. Priority Roadmap

### P0 — visible quick wins (backend already there)

1. **Group-member priority in WebUI** — ✅ **DONE**
   - `webui/src/lib/api.ts:537` — `addGroupMember(groupId, accountId, priority?)`.
   - `webui/src/components/accounts/edit-dialog.tsx` — per-group priority
     editor beside the multi-select chips, seeded from the new
     `members_detail` array on `/admin/groups/{id}/members`.
   - Backend: `ports.GroupStore.ListMembersWithPriority` + the enriched
     handler response; `AddMember` upsert semantics let the same call
     update priorities on rebind.

2. **`in_flight_jobs` leak fix** — ✅ **DONE**
   - `proxy/service.go` unlock is now `sync.Once`-wrapped with a top-level
     `defer unlock()`. Manual `unlock()` calls at the three original
     sites still fire eagerly (async release, error release, sync-poll
     release) but a panic between PickAndLock and any of them can no
     longer leak the counter.
   - `AccountStore.ResetAllInFlight` clears any leaked counters at boot
     (`cmd/higgsgo/main.go` reconciliation hop). Logged with a WARN so
     an operator sees "cleared N leaked in_flight counters on boot" if
     the previous process died mid-request.

3. **Enforce `group.max_concurrent_jobs`** — ✅ **DONE**
   - `ports.PickParams` now carries `MaxGroupInFlight` and
     `MaxConcurrentPerAccount`.
   - `PickAndLock` runs a `SUM(in_flight_jobs)` subquery inside the same
     tx as the account SELECT+UPDATE and returns
     `domain.ErrGroupConcurrencyMax` when the group cap has already been
     reached — atomic under SQLite's serialized writers.
   - `proxy.Service.resolveGroupPolicy` reads both caps from the Group
     row once per request and hands them to `PickAndLock`, replacing the
     hardcoded literal `5`.
   - HTTP layer (`internal/api/v1/videos.go:writeGenerationError`) maps
     the error to `429 pool_saturated` (retryable), distinct from `503
     no_account_available` (pool is dry).
   - Tests: `TestAccountStore_PickAndLock_GroupConcurrencyCap`,
     `TestAccountStore_PickAndLock_PerAccountCap`,
     `TestAccountStore_ResetAllInFlight`.

### P1 — correctness (drop silent no-ops)

4. **Wire group-level gates into proxy path** — ✅ **DONE (partial)**
   - `proxy.Service.resolveGroupPolicy` now compiles `AllowedModels` /
     `BlockedModels` and reads `MonthlyCreditBudget` /
     `MonthlyCreditUsed` alongside the strategy + concurrency caps
     already wired in P0-3.
   - `proxy.Service.enforceGroupGates` runs the three checks
     (blocked / allowed / budget) **before** `PickAndLock`, so a
     doomed request never consumes an in-flight slot. Invalid regex
     patterns are logged and treated as "no filter" — pre-existing
     rows with bad patterns don't 500 the caller.
   - `metering.Recorder.Groups` new field; `OnJobTerminal` calls
     `GroupStore.IncrementUsed` at every non-zero terminal so the
     budget gate self-limits. Wired in `main.go`.
   - HTTP error mapping in `v1/videos.writeGenerationError`:
     `ErrModelBlocked` → 403 `model_blocked`,
     `ErrModelNotAllowed` → 403 `model_not_allowed`,
     `ErrGroupQuotaExhausted` → 402 `group_budget_exhausted`.
   - Tests: `TestEnforceGroupGates_MatrixOfDenyAndAllow` (9 cases
     covering nil-safety, block wins over allow, exact-limit passes,
     zero-budget disables), `TestCompileGroupRegex_InvalidPatternDegrades`.
   - **Deferred** (still stored-but-not-enforced):
     `APIKey.MonthlyLimit` at pre-pick (currently checked at
     recorder-time only; monthly quota exhaustion returns after the
     job is already charged), per-key model regex, per-key rate
     limit fields. Wiring these requires threading the resolved
     APIKey into `GenerationRequest` — mechanical but out of P0/P1
     scope. Tracked as P2.
   - `internal/core/resolver/` remains uncalled: its Key layer is
     what would enable those deferred gates, but the group-level
     enforcement above delivers the immediate P1 outcome without
     restructuring the request path. The resolver package will get
     wired when we bring the Key gates online.

5. **Per-account HTTP client honoring `bound_proxy_url`** — ✅ **DONE**
   - `internal/adapters/httpclient/utls/pool.go` (`utls.Pool`) caches
     one `*utls.Client` per unique bound_proxy_url. `sync.Mutex`-guarded
     map keyed by URL; malformed URLs are remembered so we don't retry
     the build on every request. `Invalidate(url)` drops one entry so
     the failover controller (or an operator) can force a rebuild
     after fixing a broken proxy.
   - `upstream.Client.Resolver` (new field, `AccountClientResolver`
     interface) is consulted in `doWithRetry` before every request. A
     `nil` return falls back to the default client (empty bound URL
     path). Errors are logged as warnings and fall back too — a broken
     per-account proxy degrades to shared egress rather than failing
     the request outright.
   - Wired in `cmd/higgsgo/main.go`: build the default client from
     `HIGGSGO_UPSTREAM_PROXY_URL` as before, then wrap it in
     `utls.NewPool(cfg, defaultClient)` and assign the pool to
     `upstreamClient.Resolver`. Boot log now includes
     `per_account_proxy_enabled=true`.
   - Tests: `TestPool_ResolveByBoundProxy` (empty URL → fallback;
     distinct URLs → distinct cached clients),
     `TestPool_MalformedURLReturnsErrorAndCaches` (bad URL doesn't
     flood the build path),
     `TestClient_Resolver_RoutesPerAccountClient` (proves the
     resolver's client actually receives the request instead of the
     shared default), `TestClient_Resolver_FallsBackOnResolverError`
     (resolver error → default client, request still succeeds).

### P2 — user-facing honesty

6. **Account probe endpoint** — ✅ **DONE**
   - `POST /admin/accounts/{id}/probe`
     (`internal/api/admin/accounts_probe.go`) actively fetches the
     account's wallet through `upstream.Client.FetchWallet` — same
     JWT minter, same per-account proxy (P1-5), same TLS
     fingerprint the pool uses in production. A green probe is
     therefore a strong signal.
   - Response: 200 with structured JSON on both success and
     failure. `ok=true` carries balance + latency; `ok=false`
     carries `error.kind` (unauthorized / forbidden / rate_limit /
     upstream_5xx / timeout / network / internal) + `error.message`.
     No HTTP error status on upstream failure — the WebUI renders
     both outcomes through the same code path.
   - Guards: 404 when the account is gone, 409 when the account is
     `banned` (deliberately soft-deleted — probing it would signal
     "just unban and it works"), 503 `probe_disabled` when no
     upstream client is wired (distinct from "call failed").
   - 15 s outer deadline so a wedged proxy can't hang an admin
     request.
   - WebUI: `admin.probeAccount()` in `webui/src/lib/api.ts`, wired
     to both card and table probe buttons in `routes/accounts.tsx`.
     Toast shows latency + balance on success, `[kind] message` on
     failure. Loading state via `probe.isPending`.
   - Tests: `TestProbe_SuccessReportsBalanceAndLatency`,
     `TestProbe_ErrorReturnsStructuredBody` (8-case matrix
     covering every error kind incl. context timeout),
     `TestProbe_NilProberReturns503`, `TestProbe_BannedAccountReturns409`.

7. **Mock uptime removed** — ✅ **DONE**
   - `webui/src/routes/models.tsx` no longer imports or calls
     `mockUptime`. Rows without a `/admin/model-health` entry show
     an em dash and a muted "no data" bar (`generateEmptySlots`).
   - Detail sheet's uptime area shows "No probe data yet — run the
     regression ticker to populate" when the JST has no health row,
     instead of a fabricated 95-100% number.
   - `generateMockSlots` in `uptime-bar.tsx` renamed to
     `generateEmptySlots`; produces total=0 slots the bar renders
     in muted gray with "No data" tooltips. The bar stays visible
     to keep column widths stable, but the data it renders is
     honest.
   - Real time-series probe data (per-slot success/fail counts) is
     still a P3 backend addition — model-health today gives one
     aggregate `uptime_pct` per JST, not the per-slot detail the
     bar was designed for. That's a separate task.
8. **LRU → jittered tiebreaker** (see §2.3) — cheap fix for hot-spot
   ordering under concurrent bursts.

### P3 — new capability

9. **Async job lifecycle in_flight tracking** — pollworker never touches
   `in_flight_jobs`. For async jobs the counter measures "acquisition
   through CreateJob" only. Decide whether pollworker should
   increment/decrement on state transitions, or whether the counter is
   redefined as "sync-only slot usage".
10. **Inter-group spillover** — currently one API key = one closed pool.
    If Group A is exhausted, consider trying Group B in the key's binding
    list before returning 429.
11. **Weighted or lease-based load balancing** — deferred until we have
    QPS data showing LRU hot-spots are a real problem in production.

---

## 5. Registration Plugin — Monorepo Split

### 5.1 Decision

The registration flow is being moved **out of `internal/`** and into a
sibling **`plugins/register/`** sub-module. Public reverse-proxy builds
compile only the interface + stub; the real automation code lives in a
separately-versioned module gated by `-tags register`.

Rationale: the sensitive scraping / captcha / cookie code should not ship
in the public higgsgo binary. Operators who need registration compile the
full variant themselves.

### 5.2 Layout

```
higgsgo/
├── go.work                              # binds main + plugins/register
├── go.mod                               # github.com/greensheep999/higgsgo
├── internal/
│   ├── ports/registrar.go               # interface (always compiled)
│   └── adapters/registrar/higgsfield/
│       ├── higgsfield_disabled.go       # default build: stub → 503
│       └── higgsfield.go (//go:build register)
│                                        # -tags register: bridge to plugin
└── plugins/
    └── register/
        └── go.mod                       # github.com/greensheep999/higgsgo/plugins/register
                                         # separately-taggable version
```

### 5.3 Current state (2026-07-18)

- ✅ Interface defined (`internal/ports/registrar.go`).
- ✅ Admin API routes mounted (`internal/api/admin/registrations.go`,
  `server.go:386`).
- ✅ Default stub returns 503 (`higgsfield_disabled.go`).
- ❌ `higgsfield.go` under `-tags register` is `panic("TODO")` for all
  four methods. Bridge not wired.
- ❌ `plugins/register/go.mod` module path is
  `github.com/higgsgo/higgsgo/plugins/register` — mismatched with main
  module `github.com/greensheep999/higgsgo`. Needs realignment + a
  top-level `go.work` file.
- ❌ `plugins/register/adapters/{camoufox,cloak}` are `not implemented`
  placeholders.
- ❌ No sqlite registration store; migration `001` creates the table but
  no CRUD adapter reads/writes it.

### 5.4 Path to working registration

1. Add top-level `go.work` binding both modules.
2. Rewrite `plugins/register/go.mod` module path to
   `github.com/greensheep999/higgsgo/plugins/register`.
3. Import `plugins/register` from `higgsfield.go` under `-tags register`
   and delegate `Enqueue`/`GetStatus`/`List`/`Retry`.
4. Write `internal/adapters/storage/sqlite/registration_store.go` +
   inject into `plugin.Deps`.
5. Fill in one working browser adapter (camoufox first — has existing
   binary; cloak later).

---

## 6. Documentation Debt

The audit found stale claims in these docs, corrected in this pass:

- `docs/POOL-AND-CPA.md` §"Concurrency" and §"Route strategies" — see this
  file's §1 and §2 for the actual state.
- `docs/ARCHITECTURE.md` §8 "sticky per-account proxy" — false; see §1
  above.
- `docs/PLUGGABLE.md` — was written assuming `internal/plugins/*`. Rewritten
  around the monorepo split in §5.
- `docs/CHANGELOG.md` — was 13 commits behind. Refreshed.
- `docs/API_REFERENCE.md` — missing 12 admin endpoints. Backfilled.

If you find a doc that contradicts the code, update the doc *and* file an
entry here in §1's "stored but never enforced" table, then decide P0/P1/P2.
