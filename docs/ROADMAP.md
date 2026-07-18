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
| `account_group_members.priority` | Backend reads it; WebUI `addGroupMember` in `webui/src/lib/api.ts:537-541` does not pass `priority`; edit UI has no control | Add param + UI slider/input |
| `internal/core/resolver/` (config cascade) | Correct implementation of Key > Group > Account > Global precedence for concurrency / proxy / route / budget / regex / rate limit / markup | **Zero external importers** — never called from proxy path; see §3 |

### ❌ Stored but never enforced (silent no-op fields)

Fields you can set in the DB / admin API / WebUI that **have no runtime effect**:

| Field | Storage | Actual runtime behavior |
|---|---|---|
| `account.bound_proxy_url` | `account_store.go:62,175,562` | **Ignored.** Every request uses the process-level `HIGGSGO_UPSTREAM_PROXY_URL` (`cmd/higgsgo/main.go:186-204`). |
| `group.max_concurrent_jobs` | `group_store.go` | **Ignored.** `PickAndLock` never consults it. |
| `group.max_concurrent_per_account` | `group_store.go` | **Ignored.** Hardcoded `in_flight_jobs < 5` at `account_store.go:418`. |
| `group.allowed_models_regex` | `group_store.go` | **Ignored.** Only read by dead `resolver` code. |
| `group.blocked_models_regex` | `group_store.go` | **Ignored.** Same. |
| `group.monthly_credit_budget` / `monthly_credit_used` | `group_store.go` | **Ignored.** `IncrementUsed` (`group_store.go:321`) has no caller. `PickAndLock` does not check group budget. |
| `account.max_concurrent` | migration `016` | **Ignored.** Never read by pick path. |
| `AvailableSlots()` | `domain/account.go:140-149` | Const `upstreamLimit = 6` diverges from the enforced literal `5`. |
| `cfg.Pool.MaxInFlightPerAccount` | `config.go:227,329` | **Ignored** by pick path. Only surfaced by config parser. |

### ❌ UI-only placeholders (button does nothing meaningful)

| Feature | UI location | Backend state |
|---|---|---|
| Account "probe" button | `webui/src/routes/accounts.tsx:413-415,643` | No `/admin/accounts/{id}/probe` handler exists. Click toasts success. |
| Per-model uptime bars | `webui/src/routes/models.tsx:326-341` | Explicit comment `MOCK: backfill with mock values for any model not in health data`. Real health only reflects globally-triggered regression ticker output. |

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

1. **Group-member priority in WebUI** (~1-2h)
   - `webui/src/lib/api.ts:537-541` — add `priority?: number` to
     `addGroupMember` payload.
   - Add slider / numeric input in `webui/src/components/accounts/edit-dialog.tsx`
     group multi-select and in the group members admin page.
   - Backend `POST /admin/groups/{id}/members` already accepts it
     (`internal/api/admin/groups.go:279,301,308`).

2. **`in_flight_jobs` leak fix** (~1h)
   - Replace three hand-rolled `unlock()` sites in
     `internal/core/proxy/service.go:225,265,292` with `defer unlock()`.
   - On startup, run a one-shot `UPDATE accounts SET in_flight_jobs = 0`
     (or reconcile against active jobs table).

3. **Enforce `group.max_concurrent_jobs`** (~half-day)
   - Extend `PickParams` with group caps read from GroupStore.
   - Add a `SUM(in_flight_jobs)` subquery in `PickAndLock` WHERE.
   - Return `ErrPoolExhausted` (per-group variant) when the aggregate hits
     the cap; caller (v1/handler.go) turns it into `429 pool_saturated`.

### P1 — correctness (drop silent no-ops)

4. **Wire `resolver.Resolve()` into proxy path** (~1 day)
   - In `proxy.Service.Generate` after `resolveGroup`, load
     Key/Group/Account/Global config, call `resolver.Resolve()`, pass
     `ResolvedConfig` through to `PickAndLock`.
   - Migrate `PickAndLock` SQL to use `ResolvedConfig.MaxConcurrent`
     (replaces hardcoded `5`).
   - Enforce `ResolvedConfig.AllowedModels` / `BlockedModels` regex before
     enqueue.
   - Enforce `ResolvedConfig.MonthlyBudget` on both Key and Group; call
     `GroupStore.IncrementUsed` in `Metering.Recorder` on terminal-cost
     events (respect existing markup logic).

5. **Per-account HTTP client honoring `bound_proxy_url`** (~half-day-1d)
   - Move `upstream.Client` from process-level singleton to a
     per-`bound_proxy_url` cache (LRU or plain `map[string]*Client` guarded
     by `sync.RWMutex`).
   - Rebuild transport when `bound_proxy_url` changes for a given account.
   - Fall back to global proxy when the field is empty.
   - Reject accounts whose proxy URL fails a preflight `CONNECT` when the
     failover controller runs its recovery pass (optional stretch).

### P2 — user-facing honesty

6. **Kill / implement account probe** — either wire a real
   `POST /admin/accounts/{id}/probe` that hits a cheap Higgsfield endpoint
   under the account's session, or hide the button.
7. **Kill / annotate mock uptime** — `models.tsx:326-341` should either
   display a real value or clearly mark cells as "no data" instead of
   fabricating one.
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
